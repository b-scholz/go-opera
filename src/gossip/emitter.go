package gossip

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"

	"github.com/Fantom-foundation/go-lachesis/src/event_check"
	"github.com/Fantom-foundation/go-lachesis/src/event_check/basic_check"
	"github.com/Fantom-foundation/go-lachesis/src/hash"
	"github.com/Fantom-foundation/go-lachesis/src/inter"
	"github.com/Fantom-foundation/go-lachesis/src/inter/ancestor"
	"github.com/Fantom-foundation/go-lachesis/src/inter/idx"
	"github.com/Fantom-foundation/go-lachesis/src/lachesis"
)

const (
	MimetypeEvent = "application/event"
)

type Emitter struct {
	store     *Store
	engine    Consensus
	engineMu  *sync.RWMutex
	prevEpoch idx.Epoch
	txpool    txPool

	dag    *lachesis.DagConfig
	config *EmitterConfig

	am         *accounts.Manager
	coinbase   common.Address
	coinbaseMu sync.RWMutex

	gasRate         metrics.Meter
	prevEmittedTime time.Time

	onEmitted func(e *inter.Event)

	done chan struct{}
	wg   sync.WaitGroup
}

// NewEmitter creation.
func NewEmitter(
	config *Config,
	am *accounts.Manager,
	engine Consensus,
	engineMu *sync.RWMutex,
	store *Store,
	txpool txPool,
	onEmitted func(e *inter.Event),
) *Emitter {

	return &Emitter{
		dag:       &config.Net.Dag,
		config:    &config.Emitter,
		am:        am,
		gasRate:   metrics.NewMeterForced(),
		engine:    engine,
		engineMu:  engineMu,
		store:     store,
		txpool:    txpool,
		onEmitted: onEmitted,
	}
}

// StartEventEmission starts event emission.
func (em *Emitter) StartEventEmission() {
	if em.done != nil {
		return
	}
	em.done = make(chan struct{})

	em.prevEmittedTime = em.loadPrevEmitTime()

	done := em.done
	em.wg.Add(1)
	go func() {
		defer em.wg.Done()
		ticker := time.NewTicker(em.config.MinEmitInterval / 10)
		for {
			select {
			case <-ticker.C:
				if time.Since(em.prevEmittedTime) >= em.config.MinEmitInterval {
					em.EmitEvent()
				}
			case <-done:
				return
			}
		}
	}()
}

// StopEventEmission stops event emission.
func (em *Emitter) StopEventEmission() {
	if em.done == nil {
		return
	}

	close(em.done)
	em.done = nil
	em.wg.Wait()
}

// SetCoinbase sets event creator.
func (em *Emitter) SetCoinbase(addr common.Address) {
	em.coinbaseMu.Lock()
	defer em.coinbaseMu.Unlock()
	em.coinbase = addr
}

// GetCoinbase gets event creator.
func (em *Emitter) GetCoinbase() common.Address {
	em.coinbaseMu.RLock()
	defer em.coinbaseMu.RUnlock()
	return em.coinbase
}

func (em *Emitter) loadPrevEmitTime() time.Time {
	prevEventId := em.store.GetLastEvent(em.engine.GetEpoch(), em.GetCoinbase())
	if prevEventId == nil {
		return em.prevEmittedTime
	}
	prevEvent := em.store.GetEventHeader(prevEventId.Epoch(), *prevEventId)
	if prevEvent == nil {
		return em.prevEmittedTime
	}
	return prevEvent.ClaimedTime.Time()
}

func (em *Emitter) addTxs(e *inter.Event) *inter.Event {
	poolTxs, err := em.txpool.Pending()
	if err != nil {
		log.Error("Tx pool transactions fetching error", "err", err)
		return e
	}

	maxGasUsed := em.maxGasPowerToUse(e)

	for _, txs := range poolTxs {
		for _, tx := range txs {
			if tx.Gas() < e.GasPowerLeft && e.GasPowerUsed+tx.Gas() < maxGasUsed {
				e.GasPowerUsed += tx.Gas()
				e.GasPowerLeft -= tx.Gas()
				e.Transactions = append(e.Transactions, txs...)
			}
		}
	}
	// Spill txs if exceeded size limit
	// In all the "real" cases, the event will be limited by gas, not size.
	// Yet it's technically possible to construct an event which is limited by size and not by gas.
	for uint64(e.CalcSize()) > basic_check.MaxEventSize && len(e.Transactions) > 0 {
		tx := e.Transactions[len(e.Transactions)-1]
		e.GasPowerUsed -= tx.Gas()
		e.GasPowerLeft += tx.Gas()
		e.Transactions = e.Transactions[:len(e.Transactions)-1]
	}
	return e
}

// createEvent is not safe for concurrent use.
func (em *Emitter) createEvent() *inter.Event {
	coinbase := em.GetCoinbase()

	if _, ok := em.engine.GetMembers()[coinbase]; !ok {
		return nil
	}

	var (
		epoch          = em.engine.GetEpoch()
		selfParentSeq  idx.Event
		selfParentTime inter.Timestamp
		parents        hash.Events
		maxLamport     idx.Lamport
	)

	vecClock := em.engine.GetVectorIndex()

	var strategy ancestor.SearchStrategy
	if vecClock != nil {
		strategy = ancestor.NewСausalityStrategy(vecClock)
	} else {
		strategy = ancestor.NewRandomStrategy(nil)
	}

	heads := em.store.GetHeads(epoch) // events with no descendants
	selfParent := em.store.GetLastEvent(epoch, coinbase)
	_, parents = ancestor.FindBestParents(em.dag.MaxParents, heads, selfParent, strategy)

	parentHeaders := make([]*inter.EventHeaderData, len(parents))
	for i, p := range parents {
		parent := em.store.GetEventHeader(epoch, p)
		if parent == nil {
			log.Crit("Emitter: head wasn't found", "e", p.String())
		}
		parentHeaders[i] = parent
		maxLamport = idx.MaxLamport(maxLamport, parent.Lamport)
	}

	selfParentSeq = 0
	selfParentTime = 0
	var selfParentHeader *inter.EventHeaderData
	if selfParent != nil {
		selfParentHeader = parentHeaders[0]
		selfParentSeq = selfParentHeader.Seq
		selfParentTime = selfParentHeader.ClaimedTime
	}

	event := inter.NewEvent()
	event.Epoch = epoch
	event.Seq = selfParentSeq + 1
	event.Creator = coinbase

	event.Parents = parents
	event.Lamport = maxLamport + 1
	event.ClaimedTime = inter.MaxTimestamp(inter.Timestamp(time.Now().UnixNano()), selfParentTime+1)
	event.GasPowerUsed = basic_check.CalcGasPowerUsed(event)

	// set consensus fields
	event = em.engine.Prepare(event) // GasPowerLeft is calced here
	if event == nil {
		log.Warn("dropped event while emitting")
		return nil
	}

	// Add txs
	event = em.addTxs(event)

	if !em.isAllowedToEmit(event, selfParentHeader) {
		return nil
	}

	// calc Merkle root
	event.TxHash = types.DeriveSha(event.Transactions)

	// sign
	signer := func(data []byte) (sig []byte, err error) {
		acc := accounts.Account{
			Address: coinbase,
		}
		w, err := em.am.Find(acc)
		if err != nil {
			return
		}
		return w.SignData(acc, MimetypeEvent, data)
	}
	if err := event.Sign(signer); err != nil {
		log.Error("Failed to sign event", "err", err)
		return nil
	}
	// calc hash after event is fully built
	event.RecacheHash()
	event.RecacheSize()
	{
		// sanity check
		dagId := params.AllEthashProtocolChanges.ChainID
		if err := event_check.ValidateAll_test(em.dag, em.engine, types.NewEIP155Signer(dagId), event, parentHeaders); err != nil {
			log.Error("Emitted incorrect event", "err", err)
			return nil
		}
	}

	// set event name for debug
	em.nameEventForDebug(event)

	//TODO: countEmittedEvents.Inc(1)

	return event
}

func (em *Emitter) maxGasPowerToUse(e *inter.Event) uint64 {
	// No txs if power is low
	{
		threshold := em.dag.GasPower.NoTxsThreshold
		if e.GasPowerLeft <= threshold {
			return 0
		}
	}
	// Smooth TPS if power isn't big
	{
		threshold := em.dag.GasPower.GasPowerControlThreshold
		if e.GasPowerLeft <= threshold {
			maxGasUsed := uint64(float64(e.ClaimedTime.Time().Sub(em.prevEmittedTime)) * em.gasRate.Rate1() * em.config.MaxGasRateGrowthFactor)
			if maxGasUsed > basic_check.MaxGasPowerUsed {
				maxGasUsed = basic_check.MaxGasPowerUsed
			}
			return maxGasUsed
		}
	}
	return basic_check.MaxGasPowerUsed
}

func (em *Emitter) isAllowedToEmit(e *inter.Event, selfParent *inter.EventHeaderData) bool {
	// Slow down emitting if power is low
	{
		threshold := em.dag.GasPower.NoTxsThreshold
		if e.GasPowerLeft <= threshold {
			adjustedEmitInterval := em.config.MaxEmitInterval - ((em.config.MaxEmitInterval-em.config.MinEmitInterval)*time.Duration(e.GasPowerLeft))/time.Duration(threshold)
			if e.ClaimedTime.Time().Sub(em.prevEmittedTime) < adjustedEmitInterval {
				return false
			}
		}
	}
	// Forbid emitting if not enough power and power is decreasing
	{
		threshold := em.dag.GasPower.EmergencyThreshold
		if e.GasPowerLeft <= threshold {
			if !(selfParent != nil && e.GasPowerLeft >= selfParent.GasPowerLeft) {
				log.Warn("Not enough power to emit event, waiting", "power", e.GasPowerLeft, "self_parent_power", selfParent.GasPowerLeft)
				return false
			}
		}
	}

	return true
}

func (em *Emitter) EmitEvent() *inter.Event {
	em.engineMu.Lock()
	defer em.engineMu.Unlock()

	e := em.createEvent()
	if e == nil {
		return nil
	}

	if em.onEmitted != nil {
		em.onEmitted(e)
	}
	em.gasRate.Mark(int64(e.GasPowerUsed))
	em.prevEmittedTime = time.Now() // record time after connecting, to add the event processing time
	log.Info("New event emitted", "e", e.String())

	return e
}

func (em *Emitter) nameEventForDebug(e *inter.Event) {
	name := []rune(hash.GetNodeName(em.coinbase))
	if len(name) < 1 {
		return
	}

	name = name[len(name)-1:]
	hash.SetEventName(e.Hash(), fmt.Sprintf("%s%03d",
		strings.ToLower(string(name)),
		e.Seq))
}
