package lib

import (
	"bytes"
	"container/heap"
	"container/list"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/btcsuite/btcutil"
	"github.com/gernest/mention"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v3"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/sasha-s/go-deadlock"
)

// mempool.go contains all of the mempool logic for the BitClout node.

const (
	// MaxTotalTransactionSizeBytes is the maximum number of bytes the pool can store
	// across all of its transactions. Once this limit is reached, transactions must
	// be evicted from the pool based on their feerate before new transactions can be
	// added.
	MaxTotalTransactionSizeBytes = 250000000 // 250MB

	// UnconnectedTxnExpirationInterval is how long we wait before automatically removing an
	// unconnected transaction.
	UnconnectedTxnExpirationInterval = time.Minute * 5

	// The maximum number of unconnected transactions the pool will store.
	MaxUnconnectedTransactions = 10000

	// The maximum number of bytes a single unconnected transaction can take up
	MaxUnconnectedTxSizeBytes = 100000
)

var (
	// The readOnlyUtxoView will update after the number of seconds specified here OR
	// the number of transactions specified here, whichever comes first. An update
	// resets both counters.
	//
	// We make these vars rather than const for testing
	ReadOnlyUtxoViewRegenerationIntervalSeconds = float64(1.0)
	ReadOnlyUtxoViewRegenerationIntervalTxns    = int64(1000)

	// LowFeeTxLimitBytesPerTenMinutes defines the number of bytes per 10 minutes of "low fee"
	// transactions the mempool will tolerate before it starts rejecting transactions
	// that fail to meet the MinTxFeePerKBNanos threshold.
	LowFeeTxLimitBytesPerTenMinutes = 150000 // Allow 150KB per minute in low-fee txns.
)

// MempoolTx contains a transaction along with additional metadata like the
// fee and time added.
type MempoolTx struct {
	Tx *MsgBitCloutTxn

	// TxMeta is the transaction metadata
	TxMeta *TransactionMetadata

	// Hash is a hash of the transaction so we don't have to recompute
	// it all the time.
	Hash *BlockHash

	// TxSizeBytes is the cached size of the transaction.
	TxSizeBytes uint64

	// The time when the txn was added to the pool
	Added time.Time

	// The block height when the txn was added to the pool. It's generally set
	// to tip+1.
	Height uint32

	// The total fee the txn pays. Cached for efficiency reasons.
	Fee uint64

	// The fee rate of the transaction in nanos per KB.
	FeePerKB uint64

	// index is used by the heap logic to allow for modification in-place.
	index int
}

// Summary stats for a set of transactions of a specific type in the mempool.
type SummaryStats struct {
	// Number of transactions of this type in the mempool.
	Count uint32

	// Number of bytes for transactions of this type in the mempool.
	TotalBytes uint64
}

func (mempoolTx *MempoolTx) String() string {
	return fmt.Sprintf("< Added: %v, index: %d, Fee: %d, Type: %v, Hash: %v", mempoolTx.Added, mempoolTx.index, mempoolTx.Fee, mempoolTx.Tx.TxnMeta.GetTxnType(), mempoolTx.Hash)
}

// MempoolTxFeeMinHeap is a priority queue based on transaction fee rate
type MempoolTxFeeMinHeap []*MempoolTx

func (pq MempoolTxFeeMinHeap) Len() int { return len(pq) }

func (pq MempoolTxFeeMinHeap) Less(i, j int) bool {
	// We want Pop to give us the lowest-fee transactions so we use < here.
	return pq[i].FeePerKB < pq[j].FeePerKB
}

func (pq MempoolTxFeeMinHeap) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *MempoolTxFeeMinHeap) Push(x interface{}) {
	n := len(*pq)
	item := x.(*MempoolTx)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *MempoolTxFeeMinHeap) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	*pq = old[0 : n-1]
	return item
}

// UnconnectedTx is a transaction that has dependencies that we haven't added yet.
type UnconnectedTx struct {
	tx *MsgBitCloutTxn
	// The ID of the Peer who initially sent the unconnected txn. Useful for
	// removing unconnected transactions when a Peer disconnects.
	peerID     uint64
	expiration time.Time
}

// BitCloutMempool is the core mempool object. It's what any outside service should use
// to aggregate transactions and mine them into blocks.
type BitCloutMempool struct {
	// Stops the mempool's services.
	quit chan struct{}

	// A reference to a blockchain object that can be used to validate transactions before
	// adding them to the pool.
	bc *Blockchain

	// Transactions with a feerate below this threshold are outright rejected.
	minFeeRateNanosPerKB uint64

	// rateLimitFeeRateNanosPerKB defines the minimum transaction feerate in "nanos per KB"
	// before a transaction is considered for rate-limiting. Note that even if a
	// transaction with a feerate below this threshold is not rate-limited, it must
	// still have a high enough feerate to be considered as part of the mempool.
	rateLimitFeeRateNanosPerKB uint64

	mtx deadlock.RWMutex

	// poolMap contains all of the transactions that have been validated by the pool.
	// Transactions in poolMap should be directly consumable by a miner and formed into
	// a block by taking them in order of when they were Added.
	poolMap map[BlockHash]*MempoolTx
	// txFeeMinHeap organizes transactions stored in poolMap by their FeePerKB. It is used
	// in order to prevent the pool from exhausing memory due to having to store too
	// many low-fee transactions.
	txFeeMinheap MempoolTxFeeMinHeap
	// totalTxSizeBytes is the total size of all of the transactions stored in poolMap. We
	// use it to determine when the pool is nearing memory-exhaustion so we can start
	// evicting transactions.
	totalTxSizeBytes uint64
	// Stores the inputs for every transaction stored in poolMap. Used to quickly check
	// if a transaction is double-spending.
	outpoints map[UtxoKey]*MsgBitCloutTxn
	// Unconnected contains transactions whose inputs reference UTXOs that are not yet
	// present in either our UTXO database or the transactions stored in pool.
	unconnectedTxns map[BlockHash]*UnconnectedTx
	// Organizes unconnectedTxns by their UTXOs. Used when adding a transaction to determine
	// which unconnectedTxns are no longer missing parents.
	unconnectedTxnsByPrev map[UtxoKey]map[BlockHash]*MsgBitCloutTxn
	// An exponentially-decayed accumulator of "low-fee" transactions we've relayed.
	// This is used to prevent someone from flooding the network with low-fee
	// transactions.
	lowFeeTxSizeAccumulator float64
	// The UNIX time (in seconds) when the last "low-fee" transaction was relayed.
	lastLowFeeTxUnixTime int64

	// pubKeyToTxnMap stores a mapping from the public key of outputs added
	// to the mempool to the corresponding transaction that resulted in their
	// addition. It is useful for figuring out how much BitClout a particular public
	// key has available to spend.
	pubKeyToTxnMap map[PkMapKey]map[BlockHash]*MempoolTx

	// BitcoinExchange transactions that contain Bitcoin transactions that have not
	// yet been mined into a block, and therefore would fail a merkle root check.
	unminedBitcoinTxns map[BlockHash]*MempoolTx

	// The next time the unconnectTxn pool will be scanned for expired unconnectedTxns.
	nextExpireScan time.Time

	// Optional. When set, we use the BlockCypher API to detect double-spends.
	blockCypherAPIKey               string
	blockCypherCheckDoubleSpendChan chan *MsgBitCloutTxn

	// These two views are used to check whether a transaction is valid before
	// adding it to the mempool. This is done by applying the transaction to the
	// backup view, and then restoring the backup view if there's an error. In
	// the future, if we can figure out an easy way to rollback bad transactions
	// on a single view, then we won't need the second view anymore.
	backupUniversalUtxoView  *UtxoView
	universalUtxoView        *UtxoView
	universalTransactionList []*MempoolTx

	// When set, transactions are initially read from this dir and dumped
	// to this dir.
	mempoolDir string

	// Whether or not we should be computing readOnlyUtxoViews.
	generateReadOnlyUtxoView bool
	// A view that contains a *near* up-to-date snapshot of the mempool. It is
	// updated periodically after N transactions OR after M  seconds, whichever
	// comes first. It's useful because it can be obtained without acquiring a
	// lock on the mempool.
	//
	// This field isn't reset with ResetPool. It requires an explicit call to
	// UpdateReadOnlyView.
	readOnlyUtxoView *UtxoView
	// Keep a list of all transactions in the mempool. This is useful for dumping
	// to the database periodically.
	readOnlyUniversalTransactionList []*MempoolTx
	readOnlyUniversalTransactionMap  map[BlockHash]*MempoolTx
	readOnlyOutpoints                map[UtxoKey]*MsgBitCloutTxn
	// Every time the readOnlyUtxoView is updated, this is incremented. It can
	// be used by obtainers of the readOnlyUtxoView to wait until a particular
	// transaction has been run.
	//
	// This field isn't reset with ResetPool. It requires an explicit call to
	// UpdateReadOnlyView.
	readOnlyUtxoViewSequenceNumber int64
	// The total number of times we've called processTransaction. Used to
	// determine whether we should update the readOnlyUtxoView.
	//
	// This field isn't reset with ResetPool. It requires an explicit call to
	// UpdateReadOnlyView.
	totalProcessTransactionCalls int64

	// We pass a copy of the data dir flag to the tx pool so that we can instantiate
	// temp badger db instances and dump mempool txns to them.
	dataDir string
}

// See comment on RemoveUnconnectedTxn. The mempool lock must be called for writing
// when calling this function.
func (mp *BitCloutMempool) removeUnconnectedTxn(tx *MsgBitCloutTxn, removeRedeemers bool) {
	txHash := tx.Hash()
	if txHash == nil {
		// If an error occurs hashing the transaction then there's nothing to do. Just
		// log and reteurn.
		glog.Error("removeUnconnectedTxn: Problem hashing txn: ")
		return
	}
	unconnectedTxn, exists := mp.unconnectedTxns[*txHash]
	if !exists {
		return
	}

	// Remove the unconnected txn from the unconnectedTxnsByPrev index
	for _, txIn := range unconnectedTxn.tx.TxInputs {
		unconnectedTxns, exists := mp.unconnectedTxnsByPrev[UtxoKey(*txIn)]
		if exists {
			delete(unconnectedTxns, *txHash)

			// Remove the map entry altogether if there are no
			// longer any unconnectedTxns which depend on it.
			if len(unconnectedTxns) == 0 {
				delete(mp.unconnectedTxnsByPrev, UtxoKey(*txIn))
			}
		}
	}

	// Remove any unconnectedTxns that spend this txn
	if removeRedeemers {
		prevOut := BitCloutInput{TxID: *txHash}
		for txOutIdx := range tx.TxOutputs {
			prevOut.Index = uint32(txOutIdx)
			for _, unconnectedTx := range mp.unconnectedTxnsByPrev[UtxoKey(prevOut)] {
				mp.removeUnconnectedTxn(unconnectedTx, true)
			}
		}
	}

	// Delete the txn from the unconnectedTxn map
	delete(mp.unconnectedTxns, *txHash)
}

// ResetPool replaces all of the internal data associated with a pool object with the
// data of the pool object passed in. It's useful when we want to do a "scorch the earth"
// update of the pool by re-processing all of its transactions into a new pool object
// first.
//
// Note the write lock must be held before calling this function.
func (mp *BitCloutMempool) resetPool(newPool *BitCloutMempool) {
	// Replace the internal mappings of the original pool with the mappings of the new
	// pool.
	mp.poolMap = newPool.poolMap
	mp.txFeeMinheap = newPool.txFeeMinheap
	mp.totalTxSizeBytes = newPool.totalTxSizeBytes
	mp.outpoints = newPool.outpoints
	mp.pubKeyToTxnMap = newPool.pubKeyToTxnMap
	mp.unconnectedTxns = newPool.unconnectedTxns
	mp.unconnectedTxnsByPrev = newPool.unconnectedTxnsByPrev
	mp.unminedBitcoinTxns = newPool.unminedBitcoinTxns
	mp.nextExpireScan = newPool.nextExpireScan
	mp.backupUniversalUtxoView = newPool.backupUniversalUtxoView
	mp.universalUtxoView = newPool.universalUtxoView
	mp.universalTransactionList = newPool.universalTransactionList

	// We don't adjust blockCypherAPIKey or blockCypherCheckDoubleSpendChan
	// since those should be unaffected

	// We don't adjust the following fields without an explicit call to
	// UpdateReadOnlyView.
	// - runReadOnlyUtxoView bool
	// - readOnlyUtxoView *UtxoView
	// - readOnlyUtxoViewSequenceNumber int64
	// - totalProcessTransactionCalls int64
	// - readOnlyUniversalTransactionList    []*MempoolTx
	// - readOnlyUniversalTransactionMap map[BlockHash]*MempoolTx
	// - readOnlyOutpoints map[UtxoKey]*MsgBitCloutTxn
	//
	// Regenerate the view if needed.
	if mp.generateReadOnlyUtxoView {
		mp.regenerateReadOnlyView()
	}

	// Don't adjust the lowFeeTxSizeAccumulator or the lastLowFeeTxUnixTime since
	// the old values should be unaffected.
}

// UpdateAfterConnectBlock updates the mempool after a block has been added to the
// blockchain. It does this by basically removing all known transactions in the block
// from the mempool as follows:
// - Build a map of all of the transactions in the block indexed by their hash.
// - Create a new mempool object.
// - Iterate through all the transactions in the mempool and add the transactions
//   to the new pool object *only if* they don't appear in the block. Do this for
//   transactions in the pool and in the unconnectedTx pool.
// - Compute which transactions were newly-accepted into the pool by effectively diffing
//   the new pool's transactions with the old pool's transactions.
// - Once the new pool object is up-to-date, the fields of the new pool object
//   replace the fields of the original pool object.
// - Return the newly added transactions computed earlier.
//
// TODO: This is fairly inefficient but the story is the same as for
// UpdateAfterDisconnectBlock.
func (mp *BitCloutMempool) UpdateAfterConnectBlock(blk *MsgBitCloutBlock) (_txnsAddedToMempool []*MempoolTx) {
	// Protect concurrent access.
	mp.mtx.Lock()
	defer mp.mtx.Unlock()

	// Make a map of all the txns in the block except the block reward.
	txnsInBlock := make(map[BlockHash]bool)
	for _, txn := range blk.Txns[1:] {
		txHash := txn.Hash()
		txnsInBlock[*txHash] = true
	}

	// Create a new pool object. No need to set the min fees as we're just using this
	// as a temporary data structure for validation.
	//
	// Don't make the new pool object deal with the BlockCypher API.
	newPool := NewBitCloutMempool(
		mp.bc, 0, /* rateLimitFeeRateNanosPerKB */
		0,     /* minFeeRateNanosPerKB */
		"",    /*blockCypherAPIKey*/
		false, /*runReadOnlyViewUpdater*/
		"" /*dataDir*/, "")

	// Get all the transactions from the old pool object.
	oldMempoolTxns, oldUnconnectedTxns, err := mp._getTransactionsOrderedByTimeAdded()
	if err != nil {
		glog.Warning(errors.Wrapf(err, "UpdateAfterConnectBlock: "))
	}

	// Add all the txns from the old pool into the new pool unless they are already
	// present in the block.

	for _, mempoolTx := range oldMempoolTxns {
		if _, exists := txnsInBlock[*mempoolTx.Hash]; exists {
			continue
		}

		// Attempt to add the txn to the mempool as we go. If it fails that's fine.
		txnsAccepted, err := newPool.processTransaction(
			mempoolTx.Tx, true /*allowUnconnected*/, false, /*rateLimit*/
			0 /*peerID*/, false /*verifySignatures*/)
		if err != nil {
			glog.Warning(errors.Wrapf(err, "UpdateAfterConnectBlock: "))
		}
		if len(txnsAccepted) == 0 {
			glog.Warningf("UpdateAfterConnectBlock: Dropping txn %v", mempoolTx.Tx)
		}
	}

	// Add all the unconnectedTxns from the old pool into the new pool unless they are already
	// present in the block.
	for _, unconnectedTx := range oldUnconnectedTxns {
		// Only add transactions to the pool if they haven't already been added by the
		// block.
		unconnectedTxHash := unconnectedTx.tx.Hash()
		if _, exists := txnsInBlock[*unconnectedTxHash]; exists {
			continue
		}

		// Fully process unconnectedTxns
		rateLimit := false
		unconnectedTxns := true
		verifySignatures := false
		_, err := newPool.processTransaction(unconnectedTx.tx, unconnectedTxns, rateLimit, unconnectedTx.peerID, verifySignatures)
		if err != nil {
			glog.Warning(errors.Wrapf(err, "UpdateAfterConnectBlock: "))
		}
	}

	// At this point, the new pool should contain an up-to-date view of the transactions
	// that should be in the mempool after connecting this block.

	// Figure out what transactions are in the new pool but not in the old pool. These
	// are transactions that were newly-added as a result of this block clearing up some
	// dependencies and so we will likely want to relay these transactions.
	newlyAcceptedTxns := []*MempoolTx{}
	for poolHash, newMempoolTx := range newPool.poolMap {
		// No need to copy poolHash since nothing saves a reference to it.
		if _, txExistsInOldPool := mp.poolMap[poolHash]; !txExistsInOldPool {
			newlyAcceptedTxns = append(newlyAcceptedTxns, newMempoolTx)
		}
	}

	// Now set the fields on the old pool to match the new pool.
	mp.resetPool(newPool)

	// Return the newly accepted transactions now that we've fully updated our mempool.
	return newlyAcceptedTxns
}

// UpdateAfterDisconnectBlock updates the mempool to reflect that a block has been
// disconnected from the blockchain. It does this by basically adding all the
// transactions in the block back to the mempool as follows:
// - A new pool object is created containing no transactions.
// - The block's transactions are added to this new pool object. This is done in order
//   to minimize dependency-related conflicts with transactions already in the mempool.
// - Then the transactions in the original pool are layered on top of the block's
//   transactions in the new pool object. Again this is done to avoid dependency
//   issues since the ordering of <block txns> followed by <original mempool txns>
//   is much less likely to have issues.
// - Then, once the new pool object is up-to-date, the fields of the new pool object
//   replace the fields of the original pool object.
//
// This function is safe for concurrent access. It is assumed the ChainLock is
// held before this function is a accessed.
//
// TODO: This is fairly inefficient and basically only necessary because computing a
// transaction's dependencies is a little shaky. If we end up making the dependency
// detection logic more robust then we could come back here and change this so that
// we're not effectively reprocessing the entire mempool every time we have a new block.
// But until then doing it this way significantly reduces complexity and should hold up
// for a while.
func (mp *BitCloutMempool) UpdateAfterDisconnectBlock(blk *MsgBitCloutBlock) {
	// Protect concurrent access.
	mp.mtx.Lock()
	defer mp.mtx.Unlock()

	// Create a new BitCloutMempool. No need to set the min fees since we're just using
	// this as a temporary data structure for validation.
	//
	// Don't make the new pool object deal with the BlockCypher API.
	newPool := NewBitCloutMempool(mp.bc, 0, /* rateLimitFeeRateNanosPerKB */
		0, /* minFeeRateNanosPerKB */
		"" /*blockCypherAPIKey*/, false,
		"" /*dataDir*/, "")

	// Add the transactions from the block to the new pool (except for the block reward,
	// which should always be the first transaction). Break out if we encounter
	// an error.
	for _, txn := range blk.Txns[1:] {
		// For transactions being added from the block just set the peerID to zero. It
		// shouldn't matter since these transactions won't be unconnectedTxns.
		rateLimit := false
		allowUnconnectedTxns := false
		peerID := uint64(0)
		verifySignatures := false
		_, err := newPool.processTransaction(txn, allowUnconnectedTxns, rateLimit, peerID, verifySignatures)
		if err != nil {
			// Log errors but don't stop adding transactions. We do this because we'd prefer
			// to drop a transaction here or there rather than lose the whole block because
			// of one bad apple.
			glog.Warning(errors.Wrapf(err, "UpdateAfterDisconnectBlock: "))
		}
	}

	// At this point the block txns have been added to the new pool. Now we need to
	// add the txns from the original pool. Start by fetching them in slice form.
	oldMempoolTxns, oldUnconnectedTxns, err := mp._getTransactionsOrderedByTimeAdded()
	if err != nil {
		glog.Warning(errors.Wrapf(err, "UpdateAfterDisconnectBlock: "))
	}
	// Iterate through the pool transactions and add them to our new pool.

	for _, mempoolTx := range oldMempoolTxns {
		// Attempt to add the txn to the mempool as we go. If it fails that's fine.
		txnsAccepted, err := newPool.processTransaction(
			mempoolTx.Tx, true /*allowUnconnectedTxns*/, false, /*rateLimit*/
			0 /*peerID*/, false /*verifySignatures*/)
		if err != nil {
			glog.Warning(errors.Wrapf(err, "UpdateAfterDisconnectBlock: "))
		}
		if len(txnsAccepted) == 0 {
			glog.Warningf("UpdateAfterDisconnectBlock: Dropping txn %v", mempoolTx.Tx)
		}
	}

	// Iterate through the unconnectedTxns and add them to our new pool as well.
	for _, oTx := range oldUnconnectedTxns {
		rateLimit := false
		allowUnconnectedTxns := true
		verifySignatures := false
		_, err := newPool.processTransaction(oTx.tx, allowUnconnectedTxns, rateLimit, oTx.peerID, verifySignatures)
		if err != nil {
			glog.Warning(errors.Wrapf(err, "UpdateAfterDisconnectBlock: "))
		}
	}

	// At this point the new mempool should be a duplicate of the original mempool but with
	// the block's transactions added (with timestamps set before the transactions that
	// were in the original pool.

	// Replace the internal mappings of the original pool with the mappings of the new
	// pool.
	mp.resetPool(newPool)
}

// Acquires a read lock before returning the transactions.
func (mp *BitCloutMempool) GetTransactionsOrderedByTimeAdded() (_poolTxns []*MempoolTx, _unconnectedTxns []*UnconnectedTx, _err error) {
	poolTxns := []*MempoolTx{}
	poolTxns = append(poolTxns, mp.readOnlyUniversalTransactionList...)

	// Sort and return the txns.
	sort.Slice(poolTxns, func(ii, jj int) bool {
		return poolTxns[ii].Added.Before(poolTxns[jj].Added)
	})

	/*
		// TODO: We need to support unconnectedTxns as part of the readOnly infrastructure.
		unconnectedTxns := []*UnconnectedTx{}
		for _, oTx := range mp.readOnly {
			unconnectedTxns = append(unconnectedTxns, oTx)
		}
	*/

	return poolTxns, nil, nil
}

func (mp *BitCloutMempool) GetTransaction(txId *BlockHash) (txn *MempoolTx) {
	return mp.readOnlyUniversalTransactionMap[*txId]
}

// GetTransactionsOrderedByTimeAdded returns all transactions in the mempool ordered
// by when they were added to the mempool.
func (mp *BitCloutMempool) _getTransactionsOrderedByTimeAdded() (_poolTxns []*MempoolTx, _unconnectedTxns []*UnconnectedTx, _err error) {
	poolTxns := []*MempoolTx{}
	for _, mempoolTx := range mp.poolMap {
		poolTxns = append(poolTxns, mempoolTx)
	}
	// Sort the list based on when the transactions were added.
	sort.Slice(poolTxns, func(ii, jj int) bool {
		return poolTxns[ii].Added.Before(poolTxns[jj].Added)
	})

	unconnectedTxns := []*UnconnectedTx{}
	for _, oTx := range mp.unconnectedTxns {
		unconnectedTxns = append(unconnectedTxns, oTx)
	}

	return poolTxns, unconnectedTxns, nil
}

// Evicts unconnectedTxns if we're over the maximum number of unconnectedTxns allowed, or if
// unconnectedTxns have exired. Must be called with the write lock held.
func (mp *BitCloutMempool) limitNumUnconnectedTxns() error {
	if now := time.Now(); now.After(mp.nextExpireScan) {
		prevNumUnconnectedTxns := len(mp.unconnectedTxns)
		for _, unconnectedTxn := range mp.unconnectedTxns {
			if now.After(unconnectedTxn.expiration) {
				mp.removeUnconnectedTxn(unconnectedTxn.tx, true)
			}
		}

		numUnconnectedTxns := len(mp.unconnectedTxns)
		if numExpired := prevNumUnconnectedTxns - numUnconnectedTxns; numExpired > 0 {
			glog.Debugf("Expired %d unconnectedTxns (remaining: %d)", numExpired, numUnconnectedTxns)
		}
	}

	if len(mp.unconnectedTxns)+1 <= MaxUnconnectedTransactions {
		return nil
	}

	for _, otx := range mp.unconnectedTxns {
		mp.removeUnconnectedTxn(otx.tx, false)
		break
	}

	return nil
}

// Adds an unconnected txn to the pool. Must be called with the write lock held.
func (mp *BitCloutMempool) addUnconnectedTxn(tx *MsgBitCloutTxn, peerID uint64) {
	if MaxUnconnectedTransactions <= 0 {
		return
	}

	mp.limitNumUnconnectedTxns()

	txHash := tx.Hash()
	if txHash == nil {
		glog.Error(fmt.Errorf("addUnconnectedTxn: Problem hashing txn: "))
		return
	}
	mp.unconnectedTxns[*txHash] = &UnconnectedTx{
		tx:         tx,
		peerID:     peerID,
		expiration: time.Now().Add(UnconnectedTxnExpirationInterval),
	}
	for _, txIn := range tx.TxInputs {
		if _, exists := mp.unconnectedTxnsByPrev[UtxoKey(*txIn)]; !exists {
			mp.unconnectedTxnsByPrev[UtxoKey(*txIn)] =
				make(map[BlockHash]*MsgBitCloutTxn)
		}
		mp.unconnectedTxnsByPrev[UtxoKey(*txIn)][*txHash] = tx
	}

	glog.Debugf("Added unconnected transaction %v with total txns: %d)", txHash, len(mp.unconnectedTxns))
}

// Consider adding an unconnected txn to the pool. Must be called with the write lock held.
func (mp *BitCloutMempool) tryAddUnconnectedTxn(tx *MsgBitCloutTxn, peerID uint64) error {
	txBytes, err := tx.ToBytes(false)
	if err != nil {
		return errors.Wrapf(err, "tryAddUnconnectedTxn: Problem serializing txn: ")
	}
	serializedLen := len(txBytes)
	if serializedLen > MaxUnconnectedTxSizeBytes {
		return TxErrorTooLarge
	}

	mp.addUnconnectedTxn(tx, peerID)

	return nil
}

// Remove unconnectedTxns that are no longer valid after applying the passed-in txn.
func (mp *BitCloutMempool) removeUnconnectedTxnDoubleSpends(tx *MsgBitCloutTxn) {
	for _, txIn := range tx.TxInputs {
		for _, unconnectedTx := range mp.unconnectedTxnsByPrev[UtxoKey(*txIn)] {
			mp.removeUnconnectedTxn(unconnectedTx, true)
		}
	}
}

// Must be called with the write lock held.
func (mp *BitCloutMempool) isTransactionInPool(hash *BlockHash) bool {
	if _, exists := mp.poolMap[*hash]; exists {
		return true
	}

	return false
}

// Whether or not a txn is in the pool. Safe for concurrent access.
func (mp *BitCloutMempool) IsTransactionInPool(hash *BlockHash) bool {
	_, exists := mp.readOnlyUniversalTransactionMap[*hash]
	return exists
}

// Whether or not an unconnected txn is in the unconnected pool. Must be called with the write
// lock held.
func (mp *BitCloutMempool) isUnconnectedTxnInPool(hash *BlockHash) bool {
	if _, exists := mp.unconnectedTxns[*hash]; exists {
		return true
	}

	return false
}

func (mp *BitCloutMempool) DumpTxnsToDB() {
	// Dump all mempool txns into data_dir_path/temp_mempool_dump.
	err := mp.OpenTempDBAndDumpTxns()
	if err != nil {
		glog.Infof("DumpTxnsToDB: Problem opening temp db / dumping mempool txns: %v", err)
		return
	}

	// Now we shuffle the directories we created. The temp that we just created will become
	// the latest dump and the latest dump will become the previous dump.  By doing this
	// shuffle, we ensure that we always have a complete view of the mempool to load from.
	tempDir := filepath.Join(mp.mempoolDir, "temp_mempool_dump")
	previousDir := filepath.Join(mp.mempoolDir, "previous_mempool_dump")
	latestDir := filepath.Join(mp.mempoolDir, "latest_mempool_dump")

	// If latestDir exists, move latestDir --> previousDir.
	// Check that latestDir exists before trying to move it.
	_, err = os.Stat(latestDir)
	if err == nil {
		err = os.RemoveAll(previousDir)
		if err != nil {
			glog.Infof("DumpTxnsToDB: Problem deleting previous dir: %v", err)
			return
		}
		err = os.Rename(latestDir, previousDir)
		if err != nil {
			glog.Infof("DumpTxnsToDB: Problem moving latest mempool dir to previous: %v", err)
			return
		}
	}

	// Move tempDir --> latestDir. No need to delete latestDir, it was renamed above.
	err = os.Rename(tempDir, latestDir)
	if err != nil {
		glog.Infof("DumpTxnsToDB: Problem moving temp mempool dir to previous: %v", err)
		return
	}
}

// This function attempts to make the file path provided. Returns an =errors if a parent
// directory in the path does not exist or another error is encountered.
// User permissions are set to "rwx" so that it can be manipulated.
// See: https://stackoverflow.com/questions/14249467/os-mkdir-and-os-mkdirall-permission-value/31151508
func MakeDirIfNonExistent(filePath string) error {
	_, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		err = os.Mkdir(filePath, 0700)
		if err != nil {
			return fmt.Errorf("OpenTempDBAndDumpTxns: Error making dir: %v", err)
		}
	} else if err != nil {
		return fmt.Errorf("OpenTempDBAndDumpTxns: os.Stat() error: %v", err)
	}
	return nil
}

func (mp *BitCloutMempool) OpenTempDBAndDumpTxns() error {
	allTxns := mp.readOnlyUniversalTransactionList

	tempMempoolDBDir := filepath.Join(mp.mempoolDir, "temp_mempool_dump")
	glog.Infof("OpenTempDBAndDumpTxns: Opening new temp db %v", tempMempoolDBDir)
	// Make the top-level folder if it doesn't exist.
	err := MakeDirIfNonExistent(mp.mempoolDir)
	if err != nil {
		return fmt.Errorf("OpenTempDBAndDumpTxns: Error making top-level dir: %v", err)
	}
	tempMempoolDBOpts := badger.DefaultOptions(tempMempoolDBDir)
	tempMempoolDBOpts.ValueDir = tempMempoolDBDir
	tempMempoolDBOpts.MemTableSize = 1024 << 20
	tempMempoolDB, err := badger.Open(tempMempoolDBOpts)
	if err != nil {
		return fmt.Errorf("OpenTempDBAndDumpTxns: Could not open temp db to dump mempool: %v", err)
	}
	defer tempMempoolDB.Close()

	// Dump txns into the temp mempool db.
	startTime := time.Now()
	// Flush the new mempool state to the DB.
	//
	// Dump 1k txns at a time to avoid overwhelming badger
	txnsToDump := []*MempoolTx{}
	for ii, mempoolTx := range allTxns {
		txnsToDump = append(txnsToDump, mempoolTx)
		// If we're at a multiple of 1k or we're at the end of the list
		// then dump the txns to disk
		if len(txnsToDump)%1000 == 0 || ii == len(allTxns)-1 {
			glog.Infof("OpenTempDBAndDumpTxns: Dumping txns %v to %v", ii-len(txnsToDump)+1, ii)
			err := tempMempoolDB.Update(func(txn *badger.Txn) error {
				return FlushMempoolToDbWithTxn(txn, txnsToDump)
			})
			if err != nil {
				return fmt.Errorf("OpenTempDBAndDumpTxns: Error flushing mempool txns to DB: %v", err)
			}
			txnsToDump = []*MempoolTx{}
		}
	}
	endTime := time.Now()
	glog.Infof("OpenTempDBAndDumpTxns: Full txn dump of %v txns completed "+
		"in %v seconds. Safe to reboot node", len(allTxns), endTime.Sub(startTime).Seconds())
	return nil
}

// Adds a txn to the pool. This function does not do any validation, and so it should
// only be called when one is sure that a transaction is valid. Otherwise, it could
// mess up the UtxoViews that we store internally.
func (mp *BitCloutMempool) addTransaction(
	tx *MsgBitCloutTxn, height uint32, fee uint64, updateBackupView bool) (*MempoolTx, error) {

	// Add the transaction to the pool and mark the referenced outpoints
	// as spent by the pool.
	txBytes, err := tx.ToBytes(false)
	if err != nil {
		return nil, errors.Wrapf(err, "addTransaction: Problem serializing txn: ")
	}
	serializedLen := uint64(len(txBytes))

	txHash := tx.Hash()
	if txHash == nil {
		return nil, errors.Wrapf(err, "addTransaction: Problem hashing tx: ")
	}

	// If this txn would put us over our threshold then don't accept it.
	//
	// TODO: We don't replace txns in the mempool right now. Instead, a node can be
	// rebooted with a higher fee if the transactions start to get rejected due to
	// the mempool being full.
	if serializedLen+mp.totalTxSizeBytes > MaxTotalTransactionSizeBytes {
		return nil, errors.Wrapf(TxErrorInsufficientFeePriorityQueue, "addTransaction: ")
	}

	// At this point we are certain that the mempool has enough room to accomodate
	// this transaction.

	mempoolTx := &MempoolTx{
		Tx:          tx,
		Hash:        txHash,
		TxSizeBytes: uint64(serializedLen),
		Added:       time.Now(),
		Height:      height,
		Fee:         fee,
		FeePerKB:    fee * 1000 / serializedLen,
		// index will be set by the heap code.
	}

	// Add the transaction to the main pool map.
	mp.poolMap[*txHash] = mempoolTx
	// Add the transaction to the outpoints map.
	for _, txIn := range tx.TxInputs {
		mp.outpoints[UtxoKey(*txIn)] = tx
	}
	// Add the transaction to the min heap.
	heap.Push(&mp.txFeeMinheap, mempoolTx)
	// Update the size of the mempool to reflect the added transaction.
	mp.totalTxSizeBytes += mempoolTx.TxSizeBytes

	// Whenever transactions are accepted into the mempool, add a mapping
	// for each public key that they send an output to. This is useful so
	// we can find all of these outputs if, for example, the user wants
	// to know her balance while factoring in mempool transactions.
	mp._addMempoolTxToPubKeyOutputMap(mempoolTx)

	if mp.blockCypherAPIKey != "" && tx.TxnMeta.GetTxnType() == TxnTypeBitcoinExchange &&
		IsUnminedBitcoinExchange(tx.TxnMeta.(*BitcoinExchangeMetadata)) &&
		!IsForgivenBitcoinTransaction(tx) {

		go func(txnToCheck *MsgBitCloutTxn) {
			// Ten seconds is roughly how long it takes a Bitcoin transaction to fully
			// propagate through the network. See post from Satoshi in this thread:
			// https://bitcointalk.org/index.php?topic=423.20
			time.Sleep(30 * time.Second)

			// Adding the txn to this channel will trigger a double spend check.
			mp.blockCypherCheckDoubleSpendChan <- txnToCheck
		}(tx)
	}

	// Add it to the universal view. We assume the txn was already added to the
	// backup view.
	_, _, _, _, err = mp.universalUtxoView._connectTransaction(mempoolTx.Tx, mempoolTx.Hash, int64(mempoolTx.TxSizeBytes), height,
		false /*verifySignatures*/, false, /*checkMerkleProof*/
		0,
		false /*ignoreUtxos*/)
	if err != nil {
		return nil, fmt.Errorf("ERROR addTransaction: _connectTransaction " +
			"failed on universalUtxoView; this is a HUGE problem and should never happen")
	}
	// Add it to the universalTransactionList if it made it through the view
	mp.universalTransactionList = append(mp.universalTransactionList, mempoolTx)
	if updateBackupView {
		_, _, _, _, err = mp.backupUniversalUtxoView._connectTransaction(mempoolTx.Tx, mempoolTx.Hash, int64(mempoolTx.TxSizeBytes), height,
			false /*verifySignatures*/, false, /*checkMerkleProof*/
			0,
			false /*ignoreUtxos*/)
		if err != nil {
			return nil, fmt.Errorf("ERROR addTransaction: _connectTransaction " +
				"failed on backupUniversalUtxoView; this is a HUGE problem and should never happen")
		}
	}

	return mempoolTx, nil
}

func (mp *BitCloutMempool) CheckSpend(op UtxoKey) *MsgBitCloutTxn {
	txR := mp.readOnlyOutpoints[op]

	return txR
}

// GetAugmentedUtxoViewForPublicKey creates a UtxoView that has connected all of
// the transactions that could result in utxos for the passed-in public key
// plus all of the dependencies of those transactions. This is useful for
// when we want to validate a transaction that builds on a transaction that has
// not yet been mined into a block. It is also useful for when we want to fetch all
// the unspent UtxoEntrys factoring in what's been spent by transactions in
// the mempool.
func (mp *BitCloutMempool) GetAugmentedUtxoViewForPublicKey(pkBytes []byte, optionalTxn *MsgBitCloutTxn) (*UtxoView, error) {
	return mp.GetAugmentedUniversalView()
}

// GetAugmentedUniversalView creates a view that just connects everything
// in the mempool...
// TODO(performance): We should make a read-only version of the universal view that
// you can get from the mempool.
func (mp *BitCloutMempool) GetAugmentedUniversalView() (*UtxoView, error) {
	newView, err := mp.readOnlyUtxoView.CopyUtxoView()
	if err != nil {
		return nil, err
	}
	return newView, nil
}

func (mp *BitCloutMempool) FetchTransaction(txHash *BlockHash) *MempoolTx {
	if mempoolTx, exists := mp.readOnlyUniversalTransactionMap[*txHash]; exists {
		return mempoolTx
	}
	return nil
}

// TODO(performance): This function is slow, and the only reason we have it is because
// we need to validate BitcoinExchange transactions both before they have valid merkle
// proofs and *after* they have valid merkle proofs. In the latter case we can't use
// the universal view because the transaction is in the "middle" of the sorted list of
// transactions ordered by time added.
func (mp *BitCloutMempool) _quickCheckBitcoinExchangeTxn(
	tx *MsgBitCloutTxn, txHash *BlockHash, checkMerkleProof bool) (
	_fees uint64, _err error) {

	// Create a view that we'll use to validate this txn.
	//
	// Note that it is safe to use this because we expect that the blockchain
	// lock is held for the duration of this function call so there shouldn't
	// be any shifting of the db happening beneath our fee.
	utxoView, err := NewUtxoView(mp.bc.db, mp.bc.params, mp.bc.bitcoinManager)
	if err != nil {
		return 0, errors.Wrapf(err,
			"_helpConnectDepsAndFinalTxn: Problem initializing UtxoView")
	}

	// Connnect all of this transaction's dependencies to the UtxoView in order. Note
	// that we can do this because _findMempoolDependencies returns the transactions in
	// sorted order based on when transactions were added.
	bestHeight := uint32(mp.bc.blockTip().Height + 1)
	// Don't verify signatures since this transaction is already in the mempool.
	//
	// Additionally mempool verification does not require that BitcoinExchange
	// transactions meet the MinBurnWork requirement. Note that a BitcoinExchange
	// transaction will only get this far once we are positive the BitcoinManager
	// has the block corresponding to the transaction.
	// We skip verifying txn size for bitcoin exchange transactions.
	_, _, _, txFee, err := utxoView._connectTransaction(
		tx, txHash, 0, bestHeight, false,
		checkMerkleProof, /*checkMerkleProof*/
		0, false /*ignoreUtxos*/)
	if err != nil {
		// Note this can happen in odd cases where a transaction's dependency was removed
		// but the transaction depending on it was not. See the comment on
		// _findMempoolDependencies for more info on this case.
		return 0, errors.Wrapf(
			err, "_helpConnectDepsAndFinalTxn: Problem connecting "+
				"transaction dependency: ")
	}

	return txFee, nil
}

func IsUnminedBitcoinExchange(txnMeta *BitcoinExchangeMetadata) bool {
	zeroBlockHash := BlockHash{}
	return *txnMeta.BitcoinMerkleRoot == zeroBlockHash
}

func (mp *BitCloutMempool) tryAcceptBitcoinExchangeTxn(tx *MsgBitCloutTxn) (
	_missingParents []*BlockHash, _mempoolTx *MempoolTx, _err error) {

	if IsNukedBitcoinTransaction(tx) {
		nukeErr := fmt.Errorf("tryAcceptBitcoinExchangeTxn: BitcoinExchange txn %v is "+
			"being rejected because it is in the nuked list", tx.Hash())
		glog.Error(nukeErr)
		return nil, nil, nukeErr
	}

	if tx.TxnMeta.GetTxnType() != TxnTypeBitcoinExchange {
		return nil, nil, fmt.Errorf(
			"tryAcceptBitcoinExchangeTxn: Wrong txn type: %v", tx.TxnMeta.GetTxnType())
	}

	txMeta := tx.TxnMeta.(*BitcoinExchangeMetadata)

	// Verify that the txn does not have any duplicate inputs.
	//
	// TODO: This is a monkey-patch to fix a case where a clearly flawed txn made it
	// through our initial validation somehow.
	type txHashAndIndex struct {
		Hash  chainhash.Hash
		Index uint32
	}
	txInputHashes := make(map[txHashAndIndex]bool)
	for _, txIn := range txMeta.BitcoinTransaction.TxIn {
		key := txHashAndIndex{
			Hash:  txIn.PreviousOutPoint.Hash,
			Index: txIn.PreviousOutPoint.Index,
		}
		if _, exists := txInputHashes[key]; exists {
			txnBytes := bytes.Buffer{}
			txMeta.BitcoinTransaction.SerializeNoWitness(&txnBytes)

			// Return a detailed error so we can debug when this happends
			return nil, nil, fmt.Errorf(
				"tryAcceptBitcoinExchangeTxn: Error: BitcoinExchange txn "+
					"has duplicate inputs PrevHash: %v PrevIndex: %v. BitClout "+
					"hash: %v, Bitcoin hash: %v, Bitcoin txn hex: %v",
				txIn.PreviousOutPoint.Hash,
				txIn.PreviousOutPoint.Index,
				tx.Hash(), txMeta.BitcoinTransaction.TxHash(),
				hex.EncodeToString(txnBytes.Bytes()))
		}

		txInputHashes[key] = true
	}

	// Verify that the BitcoinExchange txn is not a dust transaction.
	// TODO: Is 1,000 enough for the dust threshold?
	dustOutputSatoshis := int64(1000)
	for _, txOut := range txMeta.BitcoinTransaction.TxOut {
		if txOut.Value < dustOutputSatoshis {
			// Get the Bitcoin txn bytes
			txnBytes := bytes.Buffer{}
			txMeta.BitcoinTransaction.SerializeNoWitness(&txnBytes)

			// Return a detailed error so we can debug when this happends
			return nil, nil, fmt.Errorf(
				"tryAcceptBitcoinExchangeTxn: Error: BitcoinExchange txn "+
					"output value %v is below the dust threshold %v. BitClout "+
					"hash: %v, Bitcoin hash: %v, Bitcoin txn hex: %v", txOut.Value,
				dustOutputSatoshis, tx.Hash(), txMeta.BitcoinTransaction.TxHash(),
				hex.EncodeToString(txnBytes.Bytes()))
		}
	}

	txnBytes, err := tx.ToBytes(false)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"tryAcceptBitcoinExchangeTxn: Error serializing txn: %v", err)
	}
	txnSize := int64(len(txnBytes))
	// If the transaction does not have a merkle proof then we are dealing with
	// a BitcoinExchange transaction whose corresponding Bitcoin transaction
	// has not yet been mined into a block.
	bestHeight := uint32(mp.bc.blockTip().Height + 1)
	if IsUnminedBitcoinExchange(txMeta) {
		// Just do a vanilla check and a vanilla add using the backup view.
		// Don't check merkle proofs yet.
		bestHeight = uint32(mp.bc.blockTip().Height + 1)
		_, _, _, txFee, err := mp.backupUniversalUtxoView._connectTransaction(
			tx, tx.Hash(), txnSize, bestHeight, false, /*verifySignatures*/
			false, /*checkMerkleProof*/
			0, false /*ignoreUtxos*/)
		if err != nil {
			// We need to rebuild the backup view since the _connectTransaction broke it.
			mp.rebuildBackupView()
			return nil, nil, errors.Wrapf(err, "tryAcceptBitcoinTransaction: Problem "+
				"connecting transaction after connecting dependencies: ")
		}

		// At this point we have validated that the BitcoinExchange txn can be
		// added to the mempool.

		// Add to transaction pool. We don't need to update the backup view since the call
		// above will have done this.
		mempoolTx, err := mp.addTransaction(
			tx, bestHeight, txFee, false /*updateBackupView*/)
		if err != nil {
			// We need to rebuild the backup view since the _connectTransaction broke it.
			mp.rebuildBackupView()
			return nil, nil, errors.Wrapf(err, "tryAcceptBitcoinExchangeTxn: ")
		}

		glog.Tracef("tryAcceptBitcoinExchangeTxn: Accepted unmined "+
			"transaction %v bitcoin hash: %v (pool size: %v)",
			tx.Hash(), txMeta.BitcoinTransaction.TxHash(), len(mp.poolMap))

		return nil, mempoolTx, nil
	}

	// If we get here then we are processing a BitcoinExchange transaction that
	// actually has a merkle proof on it.

	// Check to see if any of the other BitcoinExchange transactions have the
	// same hash as this transaction.
	txHash := tx.Hash()
	existingMempoolTx, hasTransaction := mp.poolMap[*txHash]

	// In this case we have a previous transaction that we need to potentially
	// replace with this one.
	if hasTransaction {
		// If the preexisting txn is a mined BitcoinExchange then don't replace it.
		if !IsUnminedBitcoinExchange(existingMempoolTx.Tx.TxnMeta.(*BitcoinExchangeMetadata)) {
			return nil, nil, TxErrorDuplicateBitcoinExchangeTxn
		}

		// Check the validity of the txn
		_, err := mp._quickCheckBitcoinExchangeTxn(
			tx, txHash, true /*checkFinalMerkleProof*/)
		if err != nil {
			return nil, nil, errors.Wrapf(
				err, "tryAcceptBitcoinExchangeTxn: "+
					"Problem connecting deps and txn: ")
		}

		// At this point we are confident that this new transaction is valid and
		// can replace the pre-existing transaction. Replace the pre-existing
		// transaction with it. This will cause it to be mined once its time has
		// come.
		existingMempoolTx.Tx = tx

		glog.Tracef("tryAcceptBitcoinExchangeTxn: Accepted REPLACEMENT "+
			"*mined* transaction %v bitcoin txhash: %v (pool size: %v)",
			tx.Hash(), txMeta.BitcoinTransaction.TxHash(), len(mp.poolMap))

		return nil, existingMempoolTx, nil
	}

	// If we get here then we don't have a pre-existing transaction so just
	// process this normally but with checkMerkleProof=true.
	// Just do a vanilla check and a vanilla add using the backup view.
	_, _, _, txFee, err := mp.backupUniversalUtxoView._connectTransaction(
		tx, tx.Hash(), txnSize, bestHeight, false, /*verifySignatures*/
		true, /*checkMerkleProof*/
		0, false /*ignoreUtxos*/)
	if err != nil {
		// We need to rebuild the backup view since the _connectTransaction broke it.
		mp.rebuildBackupView()
		return nil, nil, errors.Wrapf(err, "tryAcceptBitcoinTransaction: Problem "+
			"connecting transaction after connecting dependencies: ")
	}

	// Add to transaction pool. Don't update the backup view, since the call above
	// will have done this.
	mempoolTx, err := mp.addTransaction(tx, bestHeight, txFee, false /*updateBackupView*/)
	if err != nil {
		// We need to rebuild the backup view since the _connectTransaction broke it.
		mp.rebuildBackupView()
		return nil, nil, errors.Wrapf(err, "tryAcceptBitcoinExchangeTxn: ")
	}

	glog.Tracef("tryAcceptBitcoinExchangeTxn: Accepted *mined* "+
		"transaction %v bitcoin txhash: %v(pool size: %v)",
		tx.Hash(), txMeta.BitcoinTransaction.TxHash(), len(mp.poolMap))

	return nil, mempoolTx, nil
}

func (mp *BitCloutMempool) rebuildBackupView() {
	// We need to rebuild the backup view since the _connectTransaction broke it.
	var copyErr error
	mp.backupUniversalUtxoView, copyErr = mp.universalUtxoView.CopyUtxoView()
	if copyErr != nil {
		glog.Errorf("ERROR tryAcceptTransaction: Problem copying "+
			"view. This should NEVER happen: %v", copyErr)
	}
}

// See TryAcceptTransaction. The write lock must be held when calling this function.
//
// TODO: Allow replacing a transaction with a higher fee.
func (mp *BitCloutMempool) tryAcceptTransaction(
	tx *MsgBitCloutTxn, rateLimit bool, rejectDupUnconnected bool, verifySignatures bool) (
	_missingParents []*BlockHash, _mempoolTx *MempoolTx, _err error) {

	// Block reward transactions shouldn't appear individually
	if tx.TxnMeta != nil && tx.TxnMeta.GetTxnType() == TxnTypeBlockReward {
		return nil, nil, TxErrorIndividualBlockReward
	}

	// The BitcoinExchange logic is so customized that we break it out into its
	// own function. We do this in order to support "fast" BitClout purchases
	// in the UI that feel virtually instant without compromising on security.
	if tx.TxnMeta != nil && tx.TxnMeta.GetTxnType() == TxnTypeBitcoinExchange {
		return mp.tryAcceptBitcoinExchangeTxn(tx)
	}

	// Compute the hash of the transaction.
	txHash := tx.Hash()
	if txHash == nil {
		return nil, nil, fmt.Errorf("tryAcceptTransaction: Problem computing tx hash: ")
	}

	// Reject the txn if it already exists.
	if mp.isTransactionInPool(txHash) || (rejectDupUnconnected &&
		mp.isUnconnectedTxnInPool(txHash)) {

		return nil, nil, TxErrorDuplicate
	}

	// Iterate over the transaction's inputs. If any of them don't have utxos in the
	// UtxoView that are unspent at this point then the transaction is an unconnected
	// txn. Use a map to ensure there are no duplicates.
	missingParentsMap := make(map[BlockHash]bool)
	for _, txIn := range tx.TxInputs {
		utxoKey := UtxoKey(*txIn)
		utxoEntry := mp.universalUtxoView.GetUtxoEntryForUtxoKey(&utxoKey)
		if utxoEntry == nil {
			missingParentsMap[utxoKey.TxID] = true
		}
	}
	if len(missingParentsMap) > 0 {
		var missingParents []*BlockHash
		for txID := range missingParentsMap {
			// Must make a copy of the hash here since the iterator
			// is replaced and taking its address directly would
			// result in all of the entries pointing to the same
			// memory location and thus all be the final hash.
			hashCopy := txID
			missingParents = append(missingParents, &hashCopy)
		}
		return missingParents, nil, nil
	}

	// Attempt to add the transaction to the backup view. If it fails, reconstruct the backup
	// view and return an error.
	totalNanosPurchasedBefore := mp.backupUniversalUtxoView.NanosPurchased
	usdCentsPerBitcoinBefore := mp.backupUniversalUtxoView.GetCurrentUSDCentsPerBitcoin()
	bestHeight := uint32(mp.bc.blockTip().Height + 1)
	// We can skip verifying the transaction size as related to the minimum fee here.
	_, totalInput, totalOutput, txFee, err := mp.backupUniversalUtxoView._connectTransaction(
		tx, txHash, 0, bestHeight, verifySignatures,
		false, /*checkMerkleProof*/
		0, false /*ignoreUtxos*/)
	if err != nil {
		mp.rebuildBackupView()
		return nil, nil, errors.Wrapf(err, "tryAcceptTransaction: Problem "+
			"connecting transaction after connecting dependencies: ")
	}

	// Compute the feerate for this transaction for use below.
	txBytes, err := tx.ToBytes(false)
	if err != nil {
		mp.rebuildBackupView()
		return nil, nil, errors.Wrapf(err, "tryAcceptTransaction: Problem serializing txn: ")
	}
	serializedLen := uint64(len(txBytes))
	txFeePerKB := txFee * 1000 / serializedLen

	// Transactions with a feerate below the minimum threshold will be outright
	// rejected. This is the first line of defense against attacks against the
	// mempool.
	if rateLimit && txFeePerKB < mp.minFeeRateNanosPerKB {
		errRet := fmt.Errorf("tryAcceptTransaction: Fee rate per KB found was %d, which is below the "+
			"minimum required which is %d (= %d * %d / 1000). Total input: %d, total output: %d, "+
			"txn hash: %v, txn hex: %v",
			txFeePerKB, mp.minFeeRateNanosPerKB, mp.minFeeRateNanosPerKB, serializedLen,
			totalInput, totalOutput, txHash, hex.EncodeToString(txBytes))
		glog.Error(errRet)
		mp.rebuildBackupView()
		return nil, nil, errors.Wrapf(TxErrorInsufficientFeeMinFee, errRet.Error())
	}

	// If the feerate is below the minimum we've configured for the node, then apply
	// some rate-limiting logic to avoid stalling in situations in which someone is trying
	// to flood the network with low-value transacitons. This avoids a form of amplification
	// DDOS attack brought on by the fact that a single broadcast results in all nodes
	// communicating with each other.
	if rateLimit && txFeePerKB < mp.rateLimitFeeRateNanosPerKB {
		nowUnix := time.Now().Unix()

		// Exponentially decay the accumulator by a factor of 2 every 10m.
		mp.lowFeeTxSizeAccumulator /= math.Pow(2.0,
			float64(nowUnix-mp.lastLowFeeTxUnixTime)/(10*60))
		mp.lastLowFeeTxUnixTime = nowUnix

		// Check to see if the accumulator is over the limit.
		if mp.lowFeeTxSizeAccumulator >= float64(LowFeeTxLimitBytesPerTenMinutes) {
			mp.rebuildBackupView()
			return nil, nil, TxErrorInsufficientFeeRateLimit
		}

		// Update the accumulator and potentially log the state.
		oldTotal := mp.lowFeeTxSizeAccumulator
		mp.lowFeeTxSizeAccumulator += float64(serializedLen)
		glog.Tracef("tryAcceptTransaction: Rate limit current total ~(%v) bytes/10m, nextTotal: ~(%v) bytes/10m, "+
			"limit ~(%v) bytes/10m", oldTotal, mp.lowFeeTxSizeAccumulator, LowFeeTxLimitBytesPerTenMinutes)
	}

	// Add to transaction pool. Don't update the backup view since the call above
	// will have already done this.
	mempoolTx, err := mp.addTransaction(tx, bestHeight, txFee, false /*updateBackupUniversalView*/)
	if err != nil {
		mp.rebuildBackupView()
		return nil, nil, errors.Wrapf(err, "tryAcceptTransaction: ")
	}

	// Calculate metadata
	txnMeta, err := ComputeTransactionMetadata(tx, mp.backupUniversalUtxoView, tx.Hash(), totalNanosPurchasedBefore,
		usdCentsPerBitcoinBefore, totalInput, totalOutput, txFee, uint64(0))
	if err == nil {
		mempoolTx.TxMeta = txnMeta
	}

	glog.Tracef("tryAcceptTransaction: Accepted transaction %v (pool size: %v)", txHash,
		len(mp.poolMap))

	return nil, mempoolTx, nil
}

func ComputeTransactionMetadata(txn *MsgBitCloutTxn, utxoView *UtxoView, blockHash *BlockHash,
	totalNanosPurchasedBefore uint64, usdCentsPerBitcoinBefore uint64, totalInput uint64, totalOutput uint64,
	fees uint64, txnIndexInBlock uint64) (*TransactionMetadata, error) {

	var err error
	txnMeta := &TransactionMetadata{
		BlockHashHex:    hex.EncodeToString(blockHash[:]),
		TxnIndexInBlock: txnIndexInBlock,
		TxnType:         txn.TxnMeta.GetTxnType().String(),

		// This may be overwritten later on, for example if we're dealing with a
		// BitcoinExchange txn which doesn't set the txn.PublicKey
		TransactorPublicKeyBase58Check: PkToString(txn.PublicKey, utxoView.Params),

		// General transaction metadata
		BasicTransferTxindexMetadata: &BasicTransferTxindexMetadata{
			TotalInputNanos:  totalInput,
			TotalOutputNanos: totalOutput,
			FeeNanos:         fees,
			// TODO: This doesn't add much value, and it makes output hard to read because
			// it's so long so I'm commenting it out for now.
			// UtxoOpsDump:      spew.Sdump(utxoOps),
		},

		TxnOutputs: txn.TxOutputs,
	}

	extraData := txn.ExtraData

	// Set the affected public keys for the basic transfer.
	for _, output := range txn.TxOutputs {
		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(output.PublicKey, utxoView.Params),
			Metadata:             "BasicTransferOutput",
		})
	}

	if txn.TxnMeta.GetTxnType() == TxnTypeBitcoinExchange {
		txnMeta.BitcoinExchangeTxindexMetadata, txnMeta.TransactorPublicKeyBase58Check, err =
			_computeBitcoinExchangeFields(utxoView.Params, txn.TxnMeta.(*BitcoinExchangeMetadata),
				totalNanosPurchasedBefore, usdCentsPerBitcoinBefore)
		if err != nil {
			return nil, fmt.Errorf(
				"UpdateTxindex: Error computing BitcoinExchange txn metadata: %v", err)
		}

		// Set the nanos purchased before/after.
		txnMeta.BitcoinExchangeTxindexMetadata.TotalNanosPurchasedBefore = totalNanosPurchasedBefore
		txnMeta.BitcoinExchangeTxindexMetadata.TotalNanosPurchasedAfter = utxoView.NanosPurchased

		// Always associate BitcoinExchange txns with the burn public key. This makes it
		//		// easy to enumerate all burn txns in the block explorer.
		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: BurnPubKeyBase58Check,
			Metadata:             "BurnPublicKey",
		})
	}
	if txn.TxnMeta.GetTxnType() == TxnTypeCreatorCoin {
		// Get the txn metadata
		realTxMeta := txn.TxnMeta.(*CreatorCoinMetadataa)

		// Set the amount of the buy/sell/add
		txnMeta.CreatorCoinTxindexMetadata = &CreatorCoinTxindexMetadata{
			BitCloutToSellNanos:    realTxMeta.BitCloutToSellNanos,
			CreatorCoinToSellNanos: realTxMeta.CreatorCoinToSellNanos,
			BitCloutToAddNanos:     realTxMeta.BitCloutToAddNanos,
		}

		// Set the type of the operation.
		if realTxMeta.OperationType == CreatorCoinOperationTypeBuy {
			txnMeta.CreatorCoinTxindexMetadata.OperationType = "buy"
		} else if realTxMeta.OperationType == CreatorCoinOperationTypeSell {
			txnMeta.CreatorCoinTxindexMetadata.OperationType = "sell"
		} else {
			txnMeta.CreatorCoinTxindexMetadata.OperationType = "add"
		}

		// Set the affected public key to the owner of the creator coin so that they
		// get notified.
		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(realTxMeta.ProfilePublicKey, utxoView.Params),
			Metadata:             "CreatorPublicKey",
		})
	}
	if txn.TxnMeta.GetTxnType() == TxnTypeCreatorCoinTransfer {
		realTxMeta := txn.TxnMeta.(*CreatorCoinTransferMetadataa)
		creatorProfileEntry := utxoView.GetProfileEntryForPublicKey(realTxMeta.ProfilePublicKey)
		txnMeta.CreatorCoinTransferTxindexMetadata = &CreatorCoinTransferTxindexMetadata{
			CreatorUsername:            string(creatorProfileEntry.Username),
			CreatorCoinToTransferNanos: realTxMeta.CreatorCoinToTransferNanos,
		}

		diamondLevelBytes, hasDiamondLevel := txn.ExtraData[DiamondLevelKey]
		if hasDiamondLevel {
			diamondLevel, bytesRead := Varint(diamondLevelBytes)
			if bytesRead < 0 {
				return nil, fmt.Errorf("Update TxIndex: Error reading diamond level for txn: %v", txn.Hash().String())
			}
			txnMeta.CreatorCoinTransferTxindexMetadata.DiamondLevel = diamondLevel
			txnMeta.CreatorCoinTransferTxindexMetadata.PostHashHex = hex.EncodeToString(txn.ExtraData[DiamondPostHashKey])
		}

		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(realTxMeta.ReceiverPublicKey, utxoView.Params),
			Metadata:             "ReceiverPublicKey",
		})
	}
	if txn.TxnMeta.GetTxnType() == TxnTypeUpdateProfile {
		realTxMeta := txn.TxnMeta.(*UpdateProfileMetadata)

		txnMeta.UpdateProfileTxindexMetadata = &UpdateProfileTxindexMetadata{}
		if len(realTxMeta.ProfilePublicKey) == btcec.PubKeyBytesLenCompressed {
			txnMeta.UpdateProfileTxindexMetadata.ProfilePublicKeyBase58Check =
				PkToString(realTxMeta.ProfilePublicKey, utxoView.Params)
		}
		txnMeta.UpdateProfileTxindexMetadata.NewUsername = string(realTxMeta.NewUsername)
		txnMeta.UpdateProfileTxindexMetadata.NewDescription = string(realTxMeta.NewDescription)
		txnMeta.UpdateProfileTxindexMetadata.NewProfilePic = string(realTxMeta.NewProfilePic)
		txnMeta.UpdateProfileTxindexMetadata.NewCreatorBasisPoints = realTxMeta.NewCreatorBasisPoints
		txnMeta.UpdateProfileTxindexMetadata.NewStakeMultipleBasisPoints = realTxMeta.NewStakeMultipleBasisPoints
		txnMeta.UpdateProfileTxindexMetadata.IsHidden = realTxMeta.IsHidden

		// Add the ProfilePublicKey to the AffectedPublicKeys
		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(realTxMeta.ProfilePublicKey, utxoView.Params),
			Metadata:             "ProfilePublicKeyBase58Check",
		})
	}
	if txn.TxnMeta.GetTxnType() == TxnTypeSubmitPost {
		realTxMeta := txn.TxnMeta.(*SubmitPostMetadata)
		_ = realTxMeta

		txnMeta.SubmitPostTxindexMetadata = &SubmitPostTxindexMetadata{}
		if len(realTxMeta.PostHashToModify) == HashSizeBytes {
			txnMeta.SubmitPostTxindexMetadata.PostHashBeingModifiedHex = hex.EncodeToString(
				realTxMeta.PostHashToModify)
		}
		if len(realTxMeta.ParentStakeID) == HashSizeBytes {
			txnMeta.SubmitPostTxindexMetadata.ParentPostHashHex = hex.EncodeToString(
				realTxMeta.ParentStakeID)
		}
		// If a post hash didn't get set then the hash of the transaction itself will
		// end up being used as the post hash so set that here.
		if txnMeta.SubmitPostTxindexMetadata.PostHashBeingModifiedHex == "" {
			txnMeta.SubmitPostTxindexMetadata.PostHashBeingModifiedHex =
				hex.EncodeToString(txn.Hash()[:])
		}

		// PosterPublicKeyBase58Check = TransactorPublicKeyBase58Check

		// If ParentPostHashHex is set then get the parent posts public key and
		// mark it as affected.
		// ParentPosterPublicKeyBase58Check is in AffectedPublicKeys
		if len(realTxMeta.ParentStakeID) == HashSizeBytes {
			postHash := &BlockHash{}
			copy(postHash[:], realTxMeta.ParentStakeID)
			postEntry := utxoView.GetPostEntryForPostHash(postHash)
			if postEntry == nil {
				return nil, fmt.Errorf(
					"UpdateTxindex: Error creating SubmitPostTxindexMetadata; "+
						"missing parent post for hash %v: %v", postHash, err)
			}

			txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
				PublicKeyBase58Check: PkToString(postEntry.PosterPublicKey, utxoView.Params),
				Metadata:             "ParentPosterPublicKeyBase58Check",
			})
		}

		// The profiles that are mentioned are in the AffectedPublicKeys
		// MentionedPublicKeyBase58Check in AffectedPublicKeys. We need to
		// parse them out of the post and then look up their public keys.
		//
		// Start by trying to parse the body JSON
		bodyObj := &BitCloutBodySchema{}
		if err := json.Unmarshal(realTxMeta.Body, &bodyObj); err != nil {
			// Don't worry about bad posts unless we're debugging with high verbosity.
			glog.Tracef("UpdateTxindex: Error parsing post body for @ mentions: "+
				"%v %v", string(realTxMeta.Body), err)
		} else {
			terminators := []rune(" ,.\n&*()-_+~'\"[]{}")
			dollarTagsFound := mention.GetTagsAsUniqueStrings('$', bodyObj.Body, terminators...)
			atTagsFound := mention.GetTagsAsUniqueStrings('@', bodyObj.Body, terminators...)
			tagsFound := append(dollarTagsFound, atTagsFound...)
			for _, tag := range tagsFound {
				profileFound := utxoView.GetProfileEntryForUsername([]byte(strings.ToLower(tag)))
				// Don't worry about tags that don't line up to a profile.
				if profileFound == nil {
					continue
				}
				// If we found a profile then set it as an affected public key.
				txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
					PublicKeyBase58Check: PkToString(profileFound.PublicKey, utxoView.Params),
					Metadata:             "MentionedPublicKeyBase58Check",
				})
			}
			// Additionally, we need to check if this post is a reclout and
			// fetch the original poster
			if recloutedPostHash, isReclout := extraData[RecloutedPostHash]; isReclout {
				recloutedBlockHash := &BlockHash{}
				copy(recloutedBlockHash[:], recloutedPostHash)
				recloutPost := utxoView.GetPostEntryForPostHash(recloutedBlockHash)
				if recloutPost != nil {
					txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
						PublicKeyBase58Check: PkToString(recloutPost.PosterPublicKey, utxoView.Params),
						Metadata:             "RecloutedPublicKeyBase58Check",
					})
				}
			}
		}
	}
	if txn.TxnMeta.GetTxnType() == TxnTypeLike {
		realTxMeta := txn.TxnMeta.(*LikeMetadata)
		_ = realTxMeta

		// LikerPublicKeyBase58Check = TransactorPublicKeyBase58Check

		txnMeta.LikeTxindexMetadata = &LikeTxindexMetadata{
			IsUnlike:    realTxMeta.IsUnlike,
			PostHashHex: hex.EncodeToString(realTxMeta.LikedPostHash[:]),
		}

		// Get the public key of the poster and set it as having been affected
		// by this like.
		//
		// PosterPublicKeyBase58Check in AffectedPublicKeys
		postHash := &BlockHash{}
		copy(postHash[:], realTxMeta.LikedPostHash[:])
		postEntry := utxoView.GetPostEntryForPostHash(postHash)
		if postEntry == nil {
			return nil, fmt.Errorf(
				"UpdateTxindex: Error creating LikeTxindexMetadata; "+
					"missing post for hash %v: %v", postHash, err)
		}

		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(postEntry.PosterPublicKey, utxoView.Params),
			Metadata:             "PosterPublicKeyBase58Check",
		})
	}
	if txn.TxnMeta.GetTxnType() == TxnTypeFollow {
		realTxMeta := txn.TxnMeta.(*FollowMetadata)
		_ = realTxMeta

		txnMeta.FollowTxindexMetadata = &FollowTxindexMetadata{
			IsUnfollow: realTxMeta.IsUnfollow,
		}

		// FollowerPublicKeyBase58Check = TransactorPublicKeyBase58Check

		// FollowedPublicKeyBase58Check in AffectedPublicKeys
		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(realTxMeta.FollowedPublicKey, utxoView.Params),
			Metadata:             "FollowedPublicKeyBase58Check",
		})
	}
	if txn.TxnMeta.GetTxnType() == TxnTypePrivateMessage {
		realTxMeta := txn.TxnMeta.(*PrivateMessageMetadata)
		_ = realTxMeta

		txnMeta.PrivateMessageTxindexMetadata = &PrivateMessageTxindexMetadata{
			TimestampNanos: realTxMeta.TimestampNanos,
		}

		// SenderPublicKeyBase58Check = TransactorPublicKeyBase58Check

		// RecipientPublicKeyBase58Check in AffectedPublicKeys
		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(realTxMeta.RecipientPublicKey, utxoView.Params),
			Metadata:             "RecipientPublicKeyBase58Check",
		})
	}
	if txn.TxnMeta.GetTxnType() == TxnTypeSwapIdentity {
		realTxMeta := txn.TxnMeta.(*SwapIdentityMetadataa)
		_ = realTxMeta

		txnMeta.SwapIdentityTxindexMetadata = &SwapIdentityTxindexMetadata{
			FromPublicKeyBase58Check: PkToString(realTxMeta.FromPublicKey, utxoView.Params),
			ToPublicKeyBase58Check:   PkToString(realTxMeta.ToPublicKey, utxoView.Params),
		}

		// The to and from public keys are affected by this.

		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(realTxMeta.FromPublicKey, utxoView.Params),
			Metadata:             "FromPublicKeyBase58Check",
		})
		txnMeta.AffectedPublicKeys = append(txnMeta.AffectedPublicKeys, &AffectedPublicKey{
			PublicKeyBase58Check: PkToString(realTxMeta.ToPublicKey, utxoView.Params),
			Metadata:             "ToPublicKeyBase58Check",
		})
	}

	return txnMeta, nil
}

func _computeBitcoinExchangeFields(params *BitCloutParams,
	txMetaa *BitcoinExchangeMetadata, totalNanosPurchasedBefore uint64, usdCentsPerBitcoin uint64) (
	_btcMeta *BitcoinExchangeTxindexMetadata, _spendPkBase58Check string, _err error) {

	// Extract a public key from the BitcoinTransaction's inputs. Note that we only
	// consider P2PKH inputs to be valid. If no P2PKH inputs are found then we consider
	// the transaction as a whole to be invalid since we don't know who to credit the
	// new BitClout to. If we find more than one P2PKH input, we consider the public key
	// corresponding to the first of these inputs to be the one that will receive the
	// BitClout that will be created.
	publicKey, err := ExtractBitcoinPublicKeyFromBitcoinTransactionInputs(
		txMetaa.BitcoinTransaction, params.BitcoinBtcdParams)
	if err != nil {
		return nil, "", RuleErrorBitcoinExchangeValidPublicKeyNotFoundInInputs
	}
	// At this point, we should have extracted a public key from the Bitcoin transaction
	// that we expect to credit the newly-created BitClout to.

	// The burn address cannot create this type of transaction.
	addrFromPubKey, err := btcutil.NewAddressPubKey(
		publicKey.SerializeCompressed(), params.BitcoinBtcdParams)
	if err != nil {
		return nil, "", fmt.Errorf("_connectBitcoinExchange: Error "+
			"converting public key to Bitcoin address: %v", err)
	}
	addrString := addrFromPubKey.AddressPubKeyHash().EncodeAddress()
	if addrString == params.BitcoinBurnAddress {
		return nil, "", RuleErrorBurnAddressCannotBurnBitcoin
	}

	// Go through the transaction's outputs and count up the satoshis that are being
	// allocated to the burn address. If no Bitcoin is being sent to the burn address
	// then we consider the transaction to be invalid. Watch out for overflow as we do
	// this.
	totalBurnOutput, err := _computeBitcoinBurnOutput(
		txMetaa.BitcoinTransaction, params.BitcoinBurnAddress,
		params.BitcoinBtcdParams)
	if err != nil {
		return nil, "", RuleErrorBitcoinExchangeProblemComputingBurnOutput
	}
	if totalBurnOutput <= 0 {
		return nil, "", RuleErrorBitcoinExchangeTotalOutputLessThanOrEqualZero
	}

	// At this point we know how many satoshis were burned and we know the public key
	// that should receive the BitClout we are going to create.

	// Compute the amount of BitClout that we should create as a result of this transaction.
	nanosToCreate := CalcNanosToCreate(
		totalNanosPurchasedBefore, uint64(totalBurnOutput), usdCentsPerBitcoin)

	bitcoinTxHash := txMetaa.BitcoinTransaction.TxHash()
	return &BitcoinExchangeTxindexMetadata{
		BitcoinSpendAddress: addrString,
		SatoshisBurned:      uint64(totalBurnOutput),
		NanosCreated:        nanosToCreate,
		BitcoinTxnHash:      bitcoinTxHash.String(),
	}, PkToString(publicKey.SerializeCompressed(), params), nil
}

func ConnectTxnAndComputeTransactionMetadata(
	txn *MsgBitCloutTxn, utxoView *UtxoView, blockHash *BlockHash,
	blockHeight uint32, txnIndexInBlock uint64) (*TransactionMetadata, error) {

	totalNanosPurchasedBefore := utxoView.NanosPurchased
	usdCentsPerBitcoinBefore := utxoView.GetCurrentUSDCentsPerBitcoin()
	utxoOps, totalInput, totalOutput, fees, err := utxoView._connectTransaction(
		txn, txn.Hash(), 0, blockHeight, false, /*verifySignatures*/
		false, /*checkMerkleProof*/
		0,
		false /*ignoreUtxos*/)
	_ = utxoOps
	if err != nil {
		return nil, fmt.Errorf(
			"UpdateTxindex: Error connecting txn to UtxoView: %v", err)
	}

	return ComputeTransactionMetadata(txn, utxoView, blockHash, totalNanosPurchasedBefore,
		usdCentsPerBitcoinBefore, totalInput, totalOutput, fees, txnIndexInBlock)
}

// This is the main function used for adding a new txn to the pool. It will
// run all needed validation on the txn before adding it, and it will only
// accept the txn if these validations pass.
//
// The ChainLock must be held for reading calling this function.
func (mp *BitCloutMempool) TryAcceptTransaction(tx *MsgBitCloutTxn, rateLimit bool, verifySignatures bool) ([]*BlockHash, *MempoolTx, error) {
	// Protect concurrent access.
	mp.mtx.Lock()
	defer mp.mtx.Unlock()

	hashes, mempoolTx, err := mp.tryAcceptTransaction(tx, rateLimit, true, verifySignatures)

	return hashes, mempoolTx, err
}

// See comment on ProcessUnconnectedTransactions
func (mp *BitCloutMempool) processUnconnectedTransactions(acceptedTx *MsgBitCloutTxn, rateLimit bool, verifySignatures bool) []*MempoolTx {
	var acceptedTxns []*MempoolTx

	processList := list.New()
	processList.PushBack(acceptedTx)
	for processList.Len() > 0 {
		firstElement := processList.Remove(processList.Front())
		processItem := firstElement.(*MsgBitCloutTxn)

		processHash := processItem.Hash()
		if processHash == nil {
			glog.Error(fmt.Errorf("processUnconnectedTransactions: Problem hashing tx: "))
			return nil
		}
		prevOut := BitCloutInput{TxID: *processHash}
		for txOutIdx := range processItem.TxOutputs {
			prevOut.Index = uint32(txOutIdx)
			unconnectedTxns, exists := mp.unconnectedTxnsByPrev[UtxoKey(prevOut)]
			if !exists {
				continue
			}

			for _, tx := range unconnectedTxns {
				missing, mempoolTx, err := mp.tryAcceptTransaction(
					tx, rateLimit, false, verifySignatures)
				if err != nil {
					mp.removeUnconnectedTxn(tx, true)
					break
				}

				if len(missing) > 0 {
					continue
				}

				acceptedTxns = append(acceptedTxns, mempoolTx)
				mp.removeUnconnectedTxn(tx, false)
				processList.PushBack(tx)

				break
			}
		}
	}

	mp.removeUnconnectedTxnDoubleSpends(acceptedTx)
	for _, mempoolTx := range acceptedTxns {
		mp.removeUnconnectedTxnDoubleSpends(mempoolTx.Tx)
	}

	return acceptedTxns
}

// ProcessUnconnectedTransactions tries to see if any unconnectedTxns can now be added to the pool.
func (mp *BitCloutMempool) ProcessUnconnectedTransactions(acceptedTx *MsgBitCloutTxn, rateLimit bool, verifySignatures bool) []*MempoolTx {
	mp.mtx.Lock()
	acceptedTxns := mp.processUnconnectedTransactions(acceptedTx, rateLimit, verifySignatures)
	mp.mtx.Unlock()

	return acceptedTxns
}

func (mp *BitCloutMempool) _addTxnToPublicKeyMap(mempoolTx *MempoolTx, publicKey []byte) {
	pkMapKey := MakePkMapKey(publicKey)
	mapForPk, exists := mp.pubKeyToTxnMap[pkMapKey]
	if !exists {
		mapForPk = make(map[BlockHash]*MempoolTx)
		mp.pubKeyToTxnMap[pkMapKey] = mapForPk
	}
	mapForPk[*mempoolTx.Hash] = mempoolTx
}

func (mp *BitCloutMempool) PublicKeyTxnMap(publicKey []byte) (txnMap map[BlockHash]*MempoolTx) {
	pkMapKey := MakePkMapKey(publicKey)
	return mp.pubKeyToTxnMap[pkMapKey]
}

// TODO: This needs to consolidate with ConnectTxnAndComputeTransactionMetadata which
// does a similar thing.
func _getPublicKeysToIndexForTxn(txn *MsgBitCloutTxn, params *BitCloutParams) [][]byte {
	pubKeysToIndex := [][]byte{}

	// For each output in the transaction, add the public key.
	for _, txOut := range txn.TxOutputs {
		pubKeysToIndex = append(pubKeysToIndex, txOut.PublicKey)
	}

	// In addition to adding a mapping for each output public key, add a mapping
	// for the transaction's overall public key. This helps us find transactions
	// where this key is referenced as an input.
	if len(txn.PublicKey) == btcec.PubKeyBytesLenCompressed {
		pubKeysToIndex = append(pubKeysToIndex, txn.PublicKey)
	}

	// If the transaction is a PrivateMessage then add a mapping from the
	// recipient to this message so that it comes up when the recipient
	// creates an augmented view. Note the sender is already covered since
	// their public key is the one at the top-level transaction, which we
	// index just above.
	if txn.TxnMeta.GetTxnType() == TxnTypePrivateMessage {
		txnMeta := txn.TxnMeta.(*PrivateMessageMetadata)

		pubKeysToIndex = append(pubKeysToIndex, txnMeta.RecipientPublicKey)
	}

	if txn.TxnMeta.GetTxnType() == TxnTypeFollow {
		txnMeta := txn.TxnMeta.(*FollowMetadata)

		pubKeysToIndex = append(pubKeysToIndex, txnMeta.FollowedPublicKey)
	}

	// Index SwapIdentity txns by the pub keys embedded within the metadata
	if txn.TxnMeta.GetTxnType() == TxnTypeSwapIdentity {
		txnMeta := txn.TxnMeta.(*SwapIdentityMetadataa)

		// The ToPublicKey and the FromPublicKey can differ from the txn.PublicKey,
		// and so we need to index them separately.
		pubKeysToIndex = append(pubKeysToIndex, txnMeta.ToPublicKey)
		pubKeysToIndex = append(pubKeysToIndex, txnMeta.FromPublicKey)
	}

	if txn.TxnMeta.GetTxnType() == TxnTypeCreatorCoin {
		txnMeta := txn.TxnMeta.(*CreatorCoinMetadataa)

		// The HODLer public key is indexed when we return the txn.PublicKey
		// so we just need to additionally consider the creator.
		pubKeysToIndex = append(pubKeysToIndex, txnMeta.ProfilePublicKey)
	}

	// If the transaction is a BitcoinExchange transaction, add a mapping
	// for the implicit output created by it. Also add a mapping for the
	// burn public key so that we can easily find all burns in the block
	// explorer.
	if txn.TxnMeta.GetTxnType() == TxnTypeBitcoinExchange {
		// Add the mapping for the implicit output
		{
			txnMeta := txn.TxnMeta.(*BitcoinExchangeMetadata)
			publicKey, err := ExtractBitcoinPublicKeyFromBitcoinTransactionInputs(
				txnMeta.BitcoinTransaction, params.BitcoinBtcdParams)
			if err != nil {
				glog.Errorf("_addMempoolTxToPubKeyOutputMap: Problem extracting public key "+
					"from Bitcoin transaction for txnMeta %v", txnMeta)
			} else {
				pubKeysToIndex = append(pubKeysToIndex, publicKey.SerializeCompressed())
			}
		}

		// Add the mapping for the burn public key.
		pubKeysToIndex = append(pubKeysToIndex, MustBase58CheckDecode(BurnPubKeyBase58Check))
	}

	return pubKeysToIndex
}

func (mp *BitCloutMempool) _addMempoolTxToPubKeyOutputMap(mempoolTx *MempoolTx) {
	// Index the transaction by any associated public keys.
	publicKeysToIndex := _getPublicKeysToIndexForTxn(mempoolTx.Tx, mp.bc.params)
	for _, pkToIndex := range publicKeysToIndex {
		mp._addTxnToPublicKeyMap(mempoolTx, pkToIndex)
	}
}

func (mp *BitCloutMempool) processTransaction(
	tx *MsgBitCloutTxn, allowUnconnectedTxn, rateLimit bool,
	peerID uint64, verifySignatures bool) ([]*MempoolTx, error) {

	txHash := tx.Hash()
	if txHash == nil {
		return nil, fmt.Errorf("ProcessTransaction: Problem hashing tx")
	}
	glog.Tracef("Processing transaction %v", txHash)

	// Run validation and try to add this txn to the pool.
	missingParents, mempoolTx, err := mp.tryAcceptTransaction(
		tx, rateLimit, true, verifySignatures)
	if err != nil {
		return nil, err
	}

	// Update the readOnlyUtxoView if we've accumulated enough calls
	// to this function. This needs to be done after the tryAcceptTransaction
	// call
	if mp.generateReadOnlyUtxoView &&
		mp.totalProcessTransactionCalls%ReadOnlyUtxoViewRegenerationIntervalTxns == 0 {
		// We call the version that doesn't lock.
		mp.regenerateReadOnlyView()
	}
	// Update the total number of transactions we've processed.
	mp.totalProcessTransactionCalls += 1

	if len(missingParents) == 0 {
		newTxs := mp.processUnconnectedTransactions(tx, rateLimit, verifySignatures)
		acceptedTxs := make([]*MempoolTx, len(newTxs)+1)

		acceptedTxs[0] = mempoolTx
		copy(acceptedTxs[1:], newTxs)

		return acceptedTxs, nil
	}

	// Reject the txn if it's an unconnected txn and we're set up to reject unconnectedTxns.
	if !allowUnconnectedTxn {
		glog.Tracef("BitCloutMempool.processTransaction: TxErrorUnconnectedTxnNotAllowed: %v %v",
			tx.Hash(), tx.TxnMeta.GetTxnType())
		return nil, TxErrorUnconnectedTxnNotAllowed
	}

	// Try to add the the transaction to the pool as an unconnected txn.
	err = mp.tryAddUnconnectedTxn(tx, peerID)
	if err != nil {
		glog.Tracef("BitCloutMempool.processTransaction: Error adding transaction as unconnected txn: %v", err)
	}
	return nil, err
}

// ProcessTransaction is the main function called by outside services to potentially
// add a transaction to the mempool. It will try to add the txn to the main pool, and
// then try to add it as an unconnected txn if that fails.
func (mp *BitCloutMempool) ProcessTransaction(tx *MsgBitCloutTxn, allowUnconnectedTxn bool, rateLimit bool, peerID uint64, verifySignatures bool) ([]*MempoolTx, error) {
	// Protect concurrent access.
	mp.mtx.Lock()
	defer mp.mtx.Unlock()

	return mp.processTransaction(tx, allowUnconnectedTxn, rateLimit, peerID, verifySignatures)
}

// Returns an estimate of the number of txns in the mempool. This is an estimate because
// it looks up the number from a readOnly view, which updates at regular intervals and
// *not* every time a txn is added to the pool.
func (mp *BitCloutMempool) Count() int {
	return len(mp.readOnlyUniversalTransactionList)
}

// Returns the hashes of all the txns in the pool using the readOnly view, which could be
// slightly out of date.
func (mp *BitCloutMempool) TxHashes() []*BlockHash {
	poolMap := mp.readOnlyUniversalTransactionMap
	hashes := make([]*BlockHash, len(poolMap))
	ii := 0
	for hash := range poolMap {
		hashCopy := hash
		hashes[ii] = &hashCopy
		ii++
	}

	return hashes
}

// Returns all MempoolTxs from the readOnly view.
func (mp *BitCloutMempool) MempoolTxs() []*MempoolTx {
	poolMap := mp.readOnlyUniversalTransactionMap
	descs := make([]*MempoolTx, len(poolMap))
	i := 0
	for _, desc := range poolMap {
		descs[i] = desc
		i++
	}

	return descs
}

func (mp *BitCloutMempool) GetMempoolSummaryStats() (_summaryStatsMap map[string]*SummaryStats) {
	allTxns := mp.readOnlyUniversalTransactionList

	transactionSummaryStats := make(map[string]*SummaryStats)
	for _, mempoolTx := range allTxns {
		// Update the mempool summary stats.
		updatedSummaryStats := &SummaryStats{}
		txnType := mempoolTx.Tx.TxnMeta.GetTxnType().String()
		summaryStats := transactionSummaryStats[txnType]
		if summaryStats == nil {
			updatedSummaryStats.Count = 1
			updatedSummaryStats.TotalBytes = mempoolTx.TxSizeBytes
		} else {
			updatedSummaryStats = summaryStats
			updatedSummaryStats.Count += 1
			updatedSummaryStats.TotalBytes += mempoolTx.TxSizeBytes
		}
		transactionSummaryStats[txnType] = updatedSummaryStats

	}

	// Return the map
	return transactionSummaryStats
}

func (mp *BitCloutMempool) inefficientRemoveTransaction(tx *MsgBitCloutTxn) {
	// In this case we remove the transaction by re-adding all the txns we can
	// to the mempool except this one.
	// TODO(performance): This could be a bit slow.
	//
	// Create a new BitCloutMempool. No need to set the min fees since we're just using
	// this as a temporary data structure for validation.
	//
	// Don't make the new pool object deal with the BlockCypher API.
	newPool := NewBitCloutMempool(mp.bc, 0, /* rateLimitFeeRateNanosPerKB */
		0, /* minFeeRateNanosPerKB */
		"" /*blockCypherAPIKey*/, false,
		"" /*dataDir*/, "")
	// At this point the block txns have been added to the new pool. Now we need to
	// add the txns from the original pool. Start by fetching them in slice form.
	oldMempoolTxns, oldUnconnectedTxns, err := mp._getTransactionsOrderedByTimeAdded()
	if err != nil {
		glog.Warning(errors.Wrapf(err, "inefficientRemoveTransaction: "))
	}
	// Iterate through the pool transactions and add them to our new pool.

	for _, mempoolTx := range oldMempoolTxns {
		if *(mempoolTx.Tx.Hash()) == *(tx.Hash()) {
			continue
		}

		// Attempt to add the txn to the mempool as we go. If it fails that's fine.
		txnsAccepted, err := newPool.processTransaction(
			mempoolTx.Tx, false /*allowUnconnectedTxn*/, false, /*rateLimit*/
			0 /*peerID*/, false /*verifySignatures*/)
		if err != nil {
			glog.Warning(errors.Wrapf(err, "inefficientRemoveTransaction: "))
		}
		if len(txnsAccepted) == 0 {
			glog.Warningf("inefficientRemoveTransaction: Dropping txn %v", mempoolTx.Tx)
		}
	}
	// Iterate through the unconnectedTxns and add them to our new pool as well.
	for _, oTx := range oldUnconnectedTxns {
		rateLimit := false
		allowUnconnectedTxn := true
		verifySignatures := false
		_, err := newPool.processTransaction(oTx.tx, allowUnconnectedTxn, rateLimit, oTx.peerID, verifySignatures)
		if err != nil {
			glog.Warning(errors.Wrapf(err, "inefficientRemoveTransaction: "))
		}
	}

	// At this point the new mempool should be a duplicate of the original mempool but with
	// the non-double-spend transactions added (with timestamps set before the transactions that
	// were in the original pool.

	// Replace the internal mappings of the original pool with the mappings of the new
	// pool.
	mp.resetPool(newPool)
}

func (mp *BitCloutMempool) InefficientRemoveTransaction(tx *MsgBitCloutTxn) {
	mp.mtx.Lock()
	defer mp.mtx.Unlock()

	mp.inefficientRemoveTransaction(tx)
}

func (mp *BitCloutMempool) EvictUnminedBitcoinTransactions(bitcoinTxnHashes []string, dryRun bool) (int64, map[string]int64, []string, []string) {
	var mempoolTxns []*MempoolTx

	if !dryRun {
		mp.mtx.Lock()
		defer mp.mtx.Unlock()

		mempoolTxns = mp.universalTransactionList
	} else {
		mempoolTxns = mp.readOnlyUniversalTransactionList
	}

	// Create a new pool to apply them to.
	newPool := NewBitCloutMempool(mp.bc, 0, 0, "", false, "", "")

	isHashToEvict := func(evictHash string) bool {
		for _, txnHash := range bitcoinTxnHashes {
			if txnHash == evictHash {
				return true
			}
		}
		return false
	}

	evictedTxnsMap := make(map[string]int64)
	evictedTxnsList := []string{}
	unminedBitcoinExchangeTxns := []string{}
	for ii, mempoolTx := range mempoolTxns {
		if mempoolTx.Tx.TxnMeta.GetTxnType() == TxnTypeBitcoinExchange && IsUnminedBitcoinExchange(mempoolTx.Tx.TxnMeta.(*BitcoinExchangeMetadata)) {
			evictHash := mempoolTx.Tx.TxnMeta.(*BitcoinExchangeMetadata).BitcoinTransaction.TxHash().String()
			unminedBitcoinExchangeTxns = append(unminedBitcoinExchangeTxns, fmt.Sprintf("%s:%d", evictHash, ii))

			// Don't add transactions if they're in our list of txns to evict
			if isHashToEvict(evictHash) {
				evictedTxnsMap[mempoolTx.Tx.TxnMeta.GetTxnType().String()] += 1
				evictedTxnsList = append(evictedTxnsList, mempoolTx.Tx.Hash().String()+":"+PkToStringMainnet(mempoolTx.Tx.Hash()[:]))
				continue
			}
		}

		// Attempt to add the txn to the mempool as we go. If it fails that's fine.
		txnsAccepted, err := newPool.ProcessTransaction(
			mempoolTx.Tx, true /*allowUnconnectedTxn*/, false, /*rateLimit*/
			0 /*peerID*/, false /*verifySignatures*/)
		if err != nil {
			glog.Warning(errors.Wrapf(err, "EvictUnminedBitcoinTxns: "))
		}
		if len(txnsAccepted) == 0 {
			evictedTxnsMap[mempoolTx.Tx.TxnMeta.GetTxnType().String()] += 1
			evictedTxnsList = append(evictedTxnsList, mempoolTx.Tx.Hash().String()+":"+PkToStringMainnet(mempoolTx.Tx.Hash()[:]))
		}
	}

	newPoolTxnCount := int64(len(newPool.poolMap))

	if !dryRun {
		// Replace the existing mempool with the new pool.
		mp.resetPool(newPool)
	}

	return newPoolTxnCount, evictedTxnsMap, evictedTxnsList, unminedBitcoinExchangeTxns
}

func (mp *BitCloutMempool) StartReadOnlyUtxoViewRegenerator() {
	glog.Info("Calling StartReadOnlyUtxoViewRegenerator...")

	go func() {
		var oldSeqNum int64
	out:
		for {
			select {
			case <-time.After(time.Duration(ReadOnlyUtxoViewRegenerationIntervalSeconds) * time.Second):
				glog.Tracef("StartReadOnlyUtxoViewRegenerator: Woke up!")

				// When we wake up, only do an update if one didn't occur since before
				// we slept. Note that the number of transactions being processed can
				// also trigger an update, which is why this check is necessary.
				newSeqNum := atomic.LoadInt64(&mp.readOnlyUtxoViewSequenceNumber)
				if oldSeqNum == newSeqNum {
					glog.Tracef("StartReadOnlyUtxoViewRegenerator: Updating view at prescribed interval")
					// Acquire a read lock when we do this.
					mp.RegenerateReadOnlyView()
					glog.Tracef("StartReadOnlyUtxoViewRegenerator: Finished view update at prescribed interval")
				} else {
					glog.Tracef("StartReadOnlyUtxoViewRegenerator: View updated while sleeping; nothing to do")
				}

				// Get the sequence number before our timer hits.
				oldSeqNum = atomic.LoadInt64(&mp.readOnlyUtxoViewSequenceNumber)

			case <-mp.quit:
				break out
			}
		}
	}()
}

func (mp *BitCloutMempool) regenerateReadOnlyView() error {
	newView, err := mp.universalUtxoView.CopyUtxoView()
	if err != nil {
		return fmt.Errorf("Error generating readOnlyUtxoView: %v", err)
	}

	// Update the view and bump the sequence number. This is how callers will
	// know that the view was updated.
	mp.readOnlyUtxoView = newView

	newTxnList := []*MempoolTx{}
	txMap := make(map[BlockHash]*MempoolTx)
	for _, mempoolTx := range mp.universalTransactionList {
		newTxnList = append(newTxnList, mempoolTx)
		txMap[*mempoolTx.Hash] = mempoolTx
	}

	mp.readOnlyUniversalTransactionList = newTxnList
	mp.readOnlyUniversalTransactionMap = txMap

	atomic.AddInt64(&mp.readOnlyUtxoViewSequenceNumber, 1)
	return nil
}

func (mp *BitCloutMempool) RegenerateReadOnlyView() error {
	mp.mtx.RLock()
	defer mp.mtx.RUnlock()

	return mp.regenerateReadOnlyView()
}

func (mp *BitCloutMempool) BlockUntilReadOnlyViewRegenerated() {
	oldSeqNum := atomic.LoadInt64(&mp.readOnlyUtxoViewSequenceNumber)
	newSeqNum := oldSeqNum
	for newSeqNum == oldSeqNum {
		// Check fairly often. Not too often.
		time.Sleep(100 * time.Millisecond)

		newSeqNum = atomic.LoadInt64(&mp.readOnlyUtxoViewSequenceNumber)
	}
}

func (mp *BitCloutMempool) StartMempoolDBDumper() {
	// If we were instructed to dump txns to the db, then do so periodically
	// Note this acquired a very minimal lock on the universalTransactionList
	go func() {
	out:
		for {
			select {
			case <-time.After(30 * time.Second):
				glog.Info("StartMempoolDBDumper: Waking up! Dumping txns now...")

				// Dump the txns and time it.
				mp.DumpTxnsToDB()

			case <-mp.quit:
				break out
			}
		}
	}()
}

func (mp *BitCloutMempool) LoadTxnsFromDB() {
	glog.Infof("LoadTxnsFromDB: Loading mempool txns from db because --load_mempool_txns_from_db was set")
	startTime := time.Now()

	// The mempool shuffles dumped txns between temp, previous, and latest dirs. By dumping txns
	// to temp first, we ensure that we always have a full set of txns in latest and previous dir.
	// Note that it is possible for previousDir to exist even if latestDir does not because
	// the machine could crash after moving latest to previous. Thus, we check both.
	savedTxnsDir := filepath.Join(mp.mempoolDir, "latest_mempool_dump")
	_, err := os.Stat(savedTxnsDir)
	if os.IsNotExist(err) {
		savedTxnsDir = filepath.Join(mp.mempoolDir, "previous_mempool_dump")
		_, err = os.Stat(savedTxnsDir)
		if err != nil {
			glog.Infof("LoadTxnsFromDB: os.Stat(previousDir) error: %v", err)
			return
		}
	} else if err != nil {
		glog.Infof("LoadTxnsFromDB: os.Stat(latestDir) error: %v", err)
		return
	}

	// If we make it this far, we found a mempool dump to load.  Woohoo!
	tempMempoolDBOpts := badger.DefaultOptions(savedTxnsDir)
	tempMempoolDBOpts.ValueDir = savedTxnsDir
	tempMempoolDBOpts.MemTableSize = 1024 << 20
	glog.Infof("LoadTxnsFrom: Opening new temp db %v", savedTxnsDir)
	tempMempoolDB, err := badger.Open(tempMempoolDBOpts)
	if err != nil {
		glog.Infof("LoadTxnsFrom: Could not open temp db to dump mempool: %v", err)
		return
	}
	defer tempMempoolDB.Close()

	// Get all saved mempool transactions from the DB.
	dbMempoolTxnsOrderedByTime, err := DbGetAllMempoolTxnsSortedByTimeAdded(tempMempoolDB)
	if err != nil {
		log.Fatalf("NewBitCloutMempool: Failed to get mempoolTxs from the DB: %v", err)
	}

	for _, mempoolTxn := range dbMempoolTxnsOrderedByTime {
		_, err := mp.processTransaction(mempoolTxn, false, false, 0, false)
		if err != nil {
			// Log errors but don't stop adding transactions. We do this because we'd prefer
			// to drop a transaction here or there rather than lose the whole block because
			// of one bad apple.
			glog.Warning(errors.Wrapf(err, "NewBitCloutMempool: Not adding txn from DB "+
				"because it had an error: "))
		}
	}
	endTime := time.Now()
	glog.Infof("LoadTxnsFromDB: Loaded %v txns in %v seconds", len(dbMempoolTxnsOrderedByTime), endTime.Sub(startTime).Seconds())
}

func (mp *BitCloutMempool) Stop() {
	close(mp.quit)
}

// Create a new pool with no transactions in it.
func NewBitCloutMempool(_bc *Blockchain, _rateLimitFeerateNanosPerKB uint64,
	_minFeerateNanosPerKB uint64, _blockCypherAPIKey string,
	_runReadOnlyViewUpdater bool, _dataDir string, _mempoolDumpDir string) *BitCloutMempool {

	utxoView, _ := NewUtxoView(_bc.db, _bc.params, _bc.bitcoinManager)
	backupUtxoView, _ := NewUtxoView(_bc.db, _bc.params, _bc.bitcoinManager)
	readOnlyUtxoView, _ := NewUtxoView(_bc.db, _bc.params, _bc.bitcoinManager)
	newPool := &BitCloutMempool{
		quit:                            make(chan struct{}),
		bc:                              _bc,
		rateLimitFeeRateNanosPerKB:      _rateLimitFeerateNanosPerKB,
		minFeeRateNanosPerKB:            _minFeerateNanosPerKB,
		poolMap:                         make(map[BlockHash]*MempoolTx),
		unconnectedTxns:                 make(map[BlockHash]*UnconnectedTx),
		unconnectedTxnsByPrev:           make(map[UtxoKey]map[BlockHash]*MsgBitCloutTxn),
		outpoints:                       make(map[UtxoKey]*MsgBitCloutTxn),
		pubKeyToTxnMap:                  make(map[PkMapKey]map[BlockHash]*MempoolTx),
		unminedBitcoinTxns:              make(map[BlockHash]*MempoolTx),
		blockCypherAPIKey:               _blockCypherAPIKey,
		blockCypherCheckDoubleSpendChan: make(chan *MsgBitCloutTxn),
		backupUniversalUtxoView:         backupUtxoView,
		universalUtxoView:               utxoView,
		mempoolDir:                      _mempoolDumpDir,
		generateReadOnlyUtxoView:        _runReadOnlyViewUpdater,
		readOnlyUtxoView:                readOnlyUtxoView,
		readOnlyUniversalTransactionMap: make(map[BlockHash]*MempoolTx),
		readOnlyOutpoints:               make(map[UtxoKey]*MsgBitCloutTxn),
		dataDir:                         _dataDir,
	}

	// TODO: DELETEME: This code is no longer needed because we check for double-spends up-front.
	// It also causes sync issues between read nodes.
	//
	// If we were passed an API key, start a process to check for BitcoinExchange
	// double-spends and evict them from the mempool.
	//if newPool.blockCypherAPIKey != "" {
	//	newPool.StartBitcoinExchangeDoubleSpendChecker()
	//}

	if newPool.mempoolDir != "" {
		newPool.LoadTxnsFromDB()
	}

	// If the caller wants the readOnlyUtxoView to update periodically then kick
	// that off here.
	if newPool.generateReadOnlyUtxoView {
		newPool.StartReadOnlyUtxoViewRegenerator()
	}

	if newPool.mempoolDir != "" {
		newPool.StartMempoolDBDumper()
	}

	return newPool
}
