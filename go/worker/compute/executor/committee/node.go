package committee

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crash"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/common/pubsub"
	"github.com/oasisprotocol/oasis-core/go/common/tracing"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	roothash "github.com/oasisprotocol/oasis-core/go/roothash/api"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/block"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/commitment"
	runtimeCommittee "github.com/oasisprotocol/oasis-core/go/runtime/committee"
	"github.com/oasisprotocol/oasis-core/go/runtime/host/protocol"
	"github.com/oasisprotocol/oasis-core/go/runtime/transaction"
	scheduler "github.com/oasisprotocol/oasis-core/go/scheduler/api"
	storage "github.com/oasisprotocol/oasis-core/go/storage/api"
	commonWorker "github.com/oasisprotocol/oasis-core/go/worker/common"
	"github.com/oasisprotocol/oasis-core/go/worker/common/committee"
	"github.com/oasisprotocol/oasis-core/go/worker/common/p2p"
	mergeCommittee "github.com/oasisprotocol/oasis-core/go/worker/compute/merge/committee"
	"github.com/oasisprotocol/oasis-core/go/worker/registration"
)

var (
	errSeenNewerBlock     = errors.New("executor: seen newer block")
	errRuntimeAborted     = errors.New("executor: runtime aborted batch processing")
	errIncompatibleHeader = errors.New("executor: incompatible header")
	errInvalidReceipt     = errors.New("executor: invalid storage receipt")
	errStorageFailed      = errors.New("executor: failed to fetch from storage")
	errIncorrectRole      = errors.New("executor: incorrect role")
	errIncorrectState     = errors.New("executor: incorrect state")
	errMsgFromNonTxnSched = errors.New("executor: received txn scheduler dispatch msg from non-txn scheduler")
)

var (
	discrepancyDetectedCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oasis_worker_execution_discrepancy_detected_count",
			Help: "Number of detected execute discrepancies.",
		},
		[]string{"runtime"},
	)
	abortedBatchCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oasis_worker_aborted_batch_count",
			Help: "Number of aborted batches.",
		},
		[]string{"runtime"},
	)
	storageCommitLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "oasis_worker_storage_commit_latency",
			Help: "Latency of storage commit calls (state + outputs) (seconds).",
		},
		[]string{"runtime"},
	)
	batchReadTime = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "oasis_worker_batch_read_time",
			Help: "Time it takes to read a batch from storage (seconds).",
		},
		[]string{"runtime"},
	)
	batchProcessingTime = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "oasis_worker_batch_processing_time",
			Help: "Time it takes for a batch to finalize (seconds).",
		},
		[]string{"runtime"},
	)
	batchRuntimeProcessingTime = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "oasis_worker_batch_runtime_processing_time",
			Help: "Time it takes for a batch to be processed by the runtime (seconds).",
		},
		[]string{"runtime"},
	)
	batchSize = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "oasis_worker_batch_size",
			Help: "Number of transactions in a batch.",
		},
		[]string{"runtime"},
	)
	nodeCollectors = []prometheus.Collector{
		discrepancyDetectedCount,
		abortedBatchCount,
		storageCommitLatency,
		batchReadTime,
		batchProcessingTime,
		batchRuntimeProcessingTime,
		batchSize,
	}

	metricsOnce sync.Once
)

// Node is a committee node.
type Node struct {
	*commonWorker.RuntimeHostNode

	commonNode *committee.Node
	mergeNode  *mergeCommittee.Node

	commonCfg    commonWorker.Config
	roleProvider registration.RoleProvider

	ctx       context.Context
	cancelCtx context.CancelFunc
	stopCh    chan struct{}
	stopOnce  sync.Once
	quitCh    chan struct{}
	initCh    chan struct{}

	// Mutable and shared with common node's worker.
	// Guarded by .commonNode.CrossNode.
	state NodeState
	// Context valid until the next round.
	// Guarded by .commonNode.CrossNode.
	roundCtx       context.Context
	roundCancelCtx context.CancelFunc

	stateTransitions *pubsub.Broker
	// Bump this when we need to change what the worker selects over.
	reselect chan struct{}

	// Guarded by .commonNode.CrossNode.
	faultDetector *faultDetector

	logger *logging.Logger
}

// Name returns the service name.
func (n *Node) Name() string {
	return "committee node"
}

// Start starts the service.
func (n *Node) Start() error {
	go n.worker()
	return nil
}

// Stop halts the service.
func (n *Node) Stop() {
	n.stopOnce.Do(func() { close(n.stopCh) })
}

// Quit returns a channel that will be closed when the service terminates.
func (n *Node) Quit() <-chan struct{} {
	return n.quitCh
}

// Cleanup performs the service specific post-termination cleanup.
func (n *Node) Cleanup() {
}

// Initialized returns a channel that will be closed when the node is
// initialized and ready to service requests.
func (n *Node) Initialized() <-chan struct{} {
	return n.initCh
}

// WatchStateTransitions subscribes to the node's state transitions.
func (n *Node) WatchStateTransitions() (<-chan NodeState, *pubsub.Subscription) {
	sub := n.stateTransitions.Subscribe()
	ch := make(chan NodeState)
	sub.Unwrap(ch)

	return ch, sub
}

func (n *Node) getMetricLabels() prometheus.Labels {
	return prometheus.Labels{
		"runtime": n.commonNode.Runtime.ID().String(),
	}
}

// HandlePeerMessage implements NodeHooks.
func (n *Node) HandlePeerMessage(ctx context.Context, message *p2p.Message) (bool, error) {
	if message.SignedTxnSchedulerBatchDispatch != nil {
		crash.Here(crashPointBatchReceiveAfter)

		sbd := message.SignedTxnSchedulerBatchDispatch

		// Before opening the signed dispatch message, verify that it was
		// actually signed by the current transaction scheduler.
		epoch := n.commonNode.Group.GetEpochSnapshot()
		txsc := epoch.GetTransactionSchedulerCommittee()
		if !txsc.PublicKeys[sbd.Signature.PublicKey] {
			// Not signed by a current txn scheduler!
			return false, errMsgFromNonTxnSched
		}

		// Transaction scheduler checks out, open the signed dispatch message
		// and add it to the queue.
		var bd commitment.TxnSchedulerBatchDispatch
		if err := sbd.Open(&bd); err != nil {
			return false, err
		}

		err := n.queueBatchBlocking(ctx, bd.CommitteeID, bd.IORoot, bd.StorageSignatures, bd.Header, sbd.Signature)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (n *Node) queueBatchBlocking(
	ctx context.Context,
	committeeID hash.Hash,
	ioRootHash hash.Hash,
	storageSignatures []signature.Signature,
	hdr block.Header,
	txnSchedSig signature.Signature,
) error {
	// Quick check to see if header is compatible.
	rtID := n.commonNode.Runtime.ID()
	if !bytes.Equal(hdr.Namespace[:], rtID[:]) {
		n.logger.Warn("received incompatible header in external batch",
			"header", hdr,
		)
		return errIncompatibleHeader
	}

	// Verify storage receipt signatures.
	epoch := n.commonNode.Group.GetEpochSnapshot()
	if err := epoch.VerifyCommitteeSignatures(scheduler.KindStorage, storageSignatures); err != nil {
		n.logger.Warn("received bad storage signature",
			"err", err,
		)
		return errInvalidReceipt
	}
	// Make sure there are enough signatures.
	rt, err := n.commonNode.Runtime.RegistryDescriptor(ctx)
	if err != nil {
		n.logger.Warn("failed to fetch runtime registry descriptor",
			"err", err,
		)
		return err
	}
	if uint64(len(storageSignatures)) < rt.Storage.MinWriteReplication {
		n.logger.Warn("received external batch with not enough storage receipts",
			"min_write_replication", rt.Storage.MinWriteReplication,
			"num_receipts", len(storageSignatures),
		)
		return errInvalidReceipt
	}

	receiptBody := storage.ReceiptBody{
		Version:   1,
		Namespace: hdr.Namespace,
		Round:     hdr.Round + 1,
		Roots:     []hash.Hash{ioRootHash},
	}
	if !signature.VerifyManyToOne(storage.ReceiptSignatureContext, cbor.Marshal(receiptBody), storageSignatures) {
		n.logger.Warn("received invalid storage receipt signature in external batch")
		return errInvalidReceipt
	}

	// Fetch inputs from storage.
	ioRoot := storage.Root{
		Namespace: hdr.Namespace,
		Version:   hdr.Round + 1,
		Hash:      ioRootHash,
	}
	txs := transaction.NewTree(n.commonNode.Storage, ioRoot)
	defer txs.Close()

	readStartTime := time.Now()
	batch, err := txs.GetInputBatch(ctx)
	if err != nil || len(batch) == 0 {
		n.logger.Error("failed to fetch inputs from storage",
			"err", err,
			"io_root", ioRoot,
		)
		return errStorageFailed
	}
	batchReadTime.With(n.getMetricLabels()).Observe(time.Since(readStartTime).Seconds())

	var batchSpanCtx opentracing.SpanContext
	if batchSpan := opentracing.SpanFromContext(ctx); batchSpan != nil {
		batchSpanCtx = batchSpan.Context()
	}

	n.commonNode.CrossNode.Lock()
	defer n.commonNode.CrossNode.Unlock()
	return n.handleExternalBatchLocked(committeeID, ioRootHash, batch, batchSpanCtx, hdr, txnSchedSig, storageSignatures)
}

// HandleBatchFromTransactionSchedulerLocked processes a batch from the transaction scheduler.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleBatchFromTransactionSchedulerLocked(
	batchSpanCtx opentracing.SpanContext,
	committeeID hash.Hash,
	ioRoot hash.Hash,
	batch transaction.RawBatch,
	txnSchedSig signature.Signature,
	inputStorageSigs []signature.Signature,
) {
	epoch := n.commonNode.Group.GetEpochSnapshot()
	expectedID := epoch.GetExecutorCommitteeID()
	if !expectedID.Equal(&committeeID) {
		return
	}

	n.maybeStartProcessingBatchLocked(ioRoot, batch, batchSpanCtx, txnSchedSig, inputStorageSigs)
}

func (n *Node) bumpReselect() {
	select {
	case n.reselect <- struct{}{}:
	default:
		// If there's one already queued, we don't need to do anything.
	}
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) transitionLocked(state NodeState) {
	n.logger.Info("state transition",
		"current_state", n.state,
		"new_state", state,
	)

	// Validate state transition.
	dests := validStateTransitions[n.state.Name()]

	var valid bool
	for _, dest := range dests[:] {
		if dest == state.Name() {
			valid = true
			break
		}
	}

	if !valid {
		panic(fmt.Sprintf("invalid state transition: %s -> %s", n.state, state))
	}

	n.state = state
	n.stateTransitions.Broadcast(state)
	// Restart our worker's select in case our state-specific channels have changed.
	n.bumpReselect()
}

// HandleEpochTransitionLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleEpochTransitionLocked(epoch *committee.EpochSnapshot) {
	if epoch.IsExecutorMember() {
		n.transitionLocked(StateWaitingForBatch{})
	} else {
		n.transitionLocked(StateNotReady{})
	}
}

// HandleNewBlockEarlyLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleNewBlockEarlyLocked(blk *block.Block) {
	crash.Here(crashPointRoothashReceiveAfter)
	// If we have seen a new block while a batch was processing, we need to
	// abort it no matter what as any processed state may be invalid.
	n.abortBatchLocked(errSeenNewerBlock)
}

// HandleNewBlockLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleNewBlockLocked(blk *block.Block) {
	header := blk.Header

	// Cancel old round context, start a new one.
	if n.roundCancelCtx != nil {
		(n.roundCancelCtx)()
	}
	n.roundCtx, n.roundCancelCtx = context.WithCancel(n.ctx)

	// Perform actions based on current state.
	switch state := n.state.(type) {
	case StateWaitingForBlock:
		// Check if this was the block we were waiting for.
		if header.MostlyEqual(state.header) {
			n.logger.Info("received block needed for batch processing")
			n.maybeStartProcessingBatchLocked(state.ioRoot, state.batch, state.batchSpanCtx, state.txnSchedSig, state.inputStorageSigs)
			break
		}

		// Check if the new block is for the same or newer round than the
		// one we are waiting for. In this case, we should abort as the
		// block will never be seen.
		curRound := header.Round
		waitRound := state.header.Round
		if curRound >= waitRound {
			n.logger.Warn("seen newer block while waiting for block")
			n.transitionLocked(StateWaitingForBatch{})
			break
		}

		// Continue waiting for block.
		n.logger.Info("still waiting for block",
			"current_round", curRound,
			"wait_round", waitRound,
		)
	case StateWaitingForEvent:
		// Block finalized without the need for a backup worker.
		n.logger.Info("considering the round finalized",
			"round", blk.Header.Round,
			"header_hash", blk.Header.EncodedHash(),
		)
		n.transitionLocked(StateWaitingForBatch{})
	case StateWaitingForFinalize:
		// A new block means the round has been finalized.
		n.logger.Info("considering the round finalized",
			"round", blk.Header.Round,
			"header_hash", blk.Header.EncodedHash(),
		)
		n.transitionLocked(StateWaitingForBatch{})

		// Record time taken for successfully processing a batch.
		batchProcessingTime.With(n.getMetricLabels()).Observe(time.Since(state.batchStartTime).Seconds())
	}
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) maybeStartProcessingBatchLocked(
	ioRoot hash.Hash,
	batch transaction.RawBatch,
	batchSpanCtx opentracing.SpanContext,
	txnSchedSig signature.Signature,
	inputStorageSigs []signature.Signature,
) {
	epoch := n.commonNode.Group.GetEpochSnapshot()

	switch {
	case epoch.IsExecutorWorker():
		// Worker, start processing immediately.
		n.startProcessingBatchLocked(ioRoot, batch, batchSpanCtx, txnSchedSig, inputStorageSigs)
	case epoch.IsExecutorBackupWorker():
		// Backup worker, wait for discrepancy event.
		state, ok := n.state.(StateWaitingForBatch)
		if ok && state.pendingEvent != nil {
			// We have already received a discrepancy event, start processing immediately.
			n.logger.Info("already received a discrepancy event, start processing batch")
			n.startProcessingBatchLocked(ioRoot, batch, batchSpanCtx, txnSchedSig, inputStorageSigs)
			return
		}

		n.transitionLocked(StateWaitingForEvent{
			ioRoot:           ioRoot,
			batch:            batch,
			batchSpanCtx:     batchSpanCtx,
			txnSchedSig:      txnSchedSig,
			inputStorageSigs: inputStorageSigs,
		})
	default:
		// Currently not a member of an executor committee, log.
		n.logger.Warn("not an executor committee member, ignoring batch")
	}
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) startProcessingBatchLocked(
	ioRoot hash.Hash,
	batch transaction.RawBatch,
	batchSpanCtx opentracing.SpanContext,
	txnSchedSig signature.Signature,
	inputStorageSigs []signature.Signature,
) {
	if n.commonNode.CurrentBlock == nil {
		panic("attempted to start processing batch with a nil block")
	}

	n.logger.Debug("processing batch",
		"batch", batch,
	)

	// Create batch processing context and channel for receiving the response.
	ctx, cancel := context.WithCancel(n.ctx)
	done := make(chan *protocol.ComputedBatch, 1)

	rq := &protocol.Body{
		RuntimeExecuteTxBatchRequest: &protocol.RuntimeExecuteTxBatchRequest{
			IORoot: ioRoot,
			Inputs: batch,
			Block:  *n.commonNode.CurrentBlock,
		},
	}

	batchStartTime := time.Now()
	batchSize.With(n.getMetricLabels()).Observe(float64(len(batch)))
	n.transitionLocked(StateProcessingBatch{ioRoot, batch, batchSpanCtx, batchStartTime, cancel, done, txnSchedSig, inputStorageSigs})

	rt := n.GetHostedRuntime()
	if rt == nil {
		// This should not happen as we only register to be an executor worker
		// once the hosted runtime is ready.
		n.logger.Error("received a batch while hosted runtime is not yet initialized")
		n.abortBatchLocked(errRuntimeAborted)
		return
	}

	// Request the worker host to process a batch. This is done in a separate
	// goroutine so that the committee node can continue processing blocks.
	go func() {
		defer close(done)

		span := opentracing.StartSpan("CallBatch(rq)",
			opentracing.Tag{Key: "rq", Value: rq},
			opentracing.ChildOf(batchSpanCtx),
		)
		ctx = opentracing.ContextWithSpan(ctx, span)
		defer span.Finish()

		rtStartTime := time.Now()
		defer func() {
			batchRuntimeProcessingTime.With(n.getMetricLabels()).Observe(time.Since(rtStartTime).Seconds())
		}()

		rsp, err := rt.Call(ctx, rq)
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			// Context was canceled while the runtime was processing a request.
			n.logger.Error("batch processing aborted by context, restarting runtime")

			// Abort the runtime, so we can start processing the next batch.
			if err = rt.Abort(n.ctx, false); err != nil {
				n.logger.Error("failed to abort the runtime",
					"err", err,
				)
			}
			return
		default:
			n.logger.Error("error while sending batch processing request to runtime",
				"err", err,
			)
			return
		}
		crash.Here(crashPointBatchProcessStartAfter)

		if rsp.RuntimeExecuteTxBatchResponse == nil {
			n.logger.Error("malformed response from runtime",
				"response", rsp,
			)
			return
		}

		// Submit response to the executor worker.
		done <- &rsp.RuntimeExecuteTxBatchResponse.Batch
	}()
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) abortBatchLocked(reason error) {
	state, ok := n.state.(StateProcessingBatch)
	if !ok {
		// We can only abort if a batch is being processed.
		return
	}

	n.logger.Warn("aborting batch",
		"reason", reason,
	)

	// Cancel the batch processing context and wait for it to finish.
	state.cancel()

	crash.Here(crashPointBatchAbortAfter)

	// TODO: Return transactions to transaction scheduler.

	abortedBatchCount.With(n.getMetricLabels()).Inc()

	// After the batch has been aborted, we must wait for the round to be
	// finalized.
	n.transitionLocked(StateWaitingForFinalize{
		batchStartTime: state.batchStartTime,
	})
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) proposeBatchLocked(batch *protocol.ComputedBatch) {
	// We must be in ProcessingBatch state if we are here.
	state := n.state.(StateProcessingBatch)

	crash.Here(crashPointBatchProposeBefore)

	n.logger.Debug("proposing batch",
		"batch", batch,
	)

	epoch := n.commonNode.Group.GetEpochSnapshot()

	// Generate proposed compute results.
	proposedResults := &commitment.ComputeBody{
		CommitteeID:      epoch.GetExecutorCommitteeID(),
		Header:           batch.Header,
		RakSig:           batch.RakSig,
		TxnSchedSig:      state.txnSchedSig,
		InputRoot:        state.ioRoot,
		InputStorageSigs: state.inputStorageSigs,
	}

	// Commit I/O and state write logs to storage.
	start := time.Now()
	err := func() error {
		span, ctx := tracing.StartSpanWithContext(n.ctx, "Apply(io, state)",
			opentracing.ChildOf(state.batchSpanCtx),
		)
		defer span.Finish()

		ctx, cancel := context.WithTimeout(ctx, n.commonCfg.StorageCommitTimeout)
		defer cancel()

		lastHeader := n.commonNode.CurrentBlock.Header

		// NOTE: Order is important for verifying the receipt.
		applyOps := []storage.ApplyOp{
			// I/O root.
			storage.ApplyOp{
				SrcRound: lastHeader.Round + 1,
				SrcRoot:  state.ioRoot,
				DstRoot:  batch.Header.IORoot,
				WriteLog: batch.IOWriteLog,
			},
			// State root.
			storage.ApplyOp{
				SrcRound: lastHeader.Round,
				SrcRoot:  lastHeader.StateRoot,
				DstRoot:  batch.Header.StateRoot,
				WriteLog: batch.StateWriteLog,
			},
		}

		receipts, err := n.commonNode.Storage.ApplyBatch(ctx, &storage.ApplyBatchRequest{
			Namespace: lastHeader.Namespace,
			DstRound:  lastHeader.Round + 1,
			Ops:       applyOps,
		})
		if err != nil {
			n.logger.Error("failed to apply to storage",
				"err", err,
			)
			return err
		}

		// Verify storage receipts.
		signatures := []signature.Signature{}
		for _, receipt := range receipts {
			var receiptBody storage.ReceiptBody
			if err = receipt.Open(&receiptBody); err != nil {
				n.logger.Error("failed to open receipt",
					"receipt", receipt,
					"err", err,
				)
				return err
			}
			if err = proposedResults.VerifyStorageReceipt(lastHeader.Namespace, lastHeader.Round+1, &receiptBody); err != nil {
				n.logger.Error("failed to validate receipt body",
					"receipt body", receiptBody,
					"err", err,
				)
				return err
			}
			signatures = append(signatures, receipt.Signature)
		}
		if err := epoch.VerifyCommitteeSignatures(scheduler.KindStorage, signatures); err != nil {
			n.logger.Error("failed to validate receipt signer",
				"err", err,
			)
			return err
		}
		proposedResults.StorageSignatures = signatures

		return nil
	}()
	storageCommitLatency.With(n.getMetricLabels()).Observe(time.Since(start).Seconds())

	if err != nil {
		n.abortBatchLocked(err)
		return
	}

	// Commit.
	commit, err := commitment.SignExecutorCommitment(n.commonNode.Identity.NodeSigner, proposedResults)
	if err != nil {
		n.logger.Error("failed to sign commitment",
			"err", err,
		)
		n.abortBatchLocked(err)
		return
	}

	// Publish commitment to merge committee.
	spanPublish := opentracing.StartSpan("PublishExecuteFinished(commitment)",
		opentracing.ChildOf(state.batchSpanCtx),
	)
	err = n.commonNode.Group.PublishExecuteFinished(state.batchSpanCtx, commit)
	if err != nil {
		spanPublish.Finish()
		n.logger.Error("failed to publish results to committee",
			"err", err,
		)
		n.abortBatchLocked(err)
		return
	}
	spanPublish.Finish()

	// TODO: Add crash point.

	// Set up the fault detector so that we can submit the commitment independently from any other
	// merge nodes in case a fault is detected (which would indicate that the entire merge committee
	// is faulty).
	n.faultDetector = newFaultDetector(n.roundCtx, n.commonNode.Runtime, commit, newNodeFaultSubmitter(n))

	n.transitionLocked(StateWaitingForFinalize{
		batchStartTime: state.batchStartTime,
	})

	if epoch.IsMergeMember() {
		if n.mergeNode == nil {
			n.logger.Error("scheduler says we are a merge worker, but we are not")
		} else {
			n.mergeNode.HandleResultsFromExecutorWorkerLocked(state.batchSpanCtx, commit)
		}
	}

	crash.Here(crashPointBatchProposeAfter)
}

// HandleNewEventLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleNewEventLocked(ev *roothash.Event) {
	// In case a fault detector exists, notify it of events.
	if n.faultDetector != nil {
		n.faultDetector.notify(ev)
	}

	dis := ev.ExecutionDiscrepancyDetected
	if dis == nil {
		// Ignore other events.
		return
	}

	// Check if the discrepancy occurred in our committee.
	epoch := n.commonNode.Group.GetEpochSnapshot()
	expectedID := epoch.GetExecutorCommitteeID()
	if !expectedID.Equal(&dis.CommitteeID) {
		n.logger.Debug("ignoring discrepancy event for a different committee",
			"expected_committee", expectedID,
			"committee", dis.CommitteeID,
		)
		return
	}

	n.logger.Warn("execution discrepancy detected",
		"committee_id", dis.CommitteeID,
	)

	crash.Here(crashPointDiscrepancyDetectedAfter)

	discrepancyDetectedCount.With(n.getMetricLabels()).Inc()

	if !n.commonNode.Group.GetEpochSnapshot().IsExecutorBackupWorker() {
		return
	}

	var state StateWaitingForEvent
	switch s := n.state.(type) {
	case StateWaitingForBatch:
		// Discrepancy detected event received before the batch. We need to
		// record the received event and keep waiting for the batch.
		s.pendingEvent = dis
		n.transitionLocked(s)
		return
	case StateWaitingForEvent:
		state = s
	default:
		n.logger.Warn("ignoring received discrepancy event in incorrect state",
			"state", s,
		)
		return
	}

	// Backup worker, start processing a batch.
	n.logger.Info("backup worker activating and processing batch")
	n.startProcessingBatchLocked(state.ioRoot, state.batch, state.batchSpanCtx, state.txnSchedSig, state.inputStorageSigs)
}

// HandleNodeUpdateLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleNodeUpdateLocked(update *runtimeCommittee.NodeUpdate, snapshot *committee.EpochSnapshot) {
	// Nothing to do here.
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) handleExternalBatchLocked(
	committeeID hash.Hash,
	ioRoot hash.Hash,
	batch transaction.RawBatch,
	batchSpanCtx opentracing.SpanContext,
	hdr block.Header,
	txnSchedSig signature.Signature,
	inputStorageSigs []signature.Signature,
) error {
	// If we are not waiting for a batch, don't do anything.
	if _, ok := n.state.(StateWaitingForBatch); !ok {
		return errIncorrectState
	}

	epoch := n.commonNode.Group.GetEpochSnapshot()

	// We can only receive external batches if we are an executor member.
	if !epoch.IsExecutorMember() {
		n.logger.Error("got external batch while in incorrect role")
		return errIncorrectRole
	}

	// We only accept batches for our own committee.
	expectedID := epoch.GetExecutorCommitteeID()
	if !expectedID.Equal(&committeeID) {
		n.logger.Error("got external batch for a different executor committee",
			"expected_committee", expectedID,
			"committee", committeeID,
		)
		return nil
	}

	// Check if we have the correct block -- in this case, start processing the batch.
	if n.commonNode.CurrentBlock.Header.MostlyEqual(&hdr) {
		n.maybeStartProcessingBatchLocked(ioRoot, batch, batchSpanCtx, txnSchedSig, inputStorageSigs)
		return nil
	}

	// Check if the current block is older than what is expected we base our batch
	// on. In case it is equal or newer, but different, discard the batch.
	curRound := n.commonNode.CurrentBlock.Header.Round
	waitRound := hdr.Round
	if curRound >= waitRound {
		n.logger.Warn("got external batch based on incompatible header",
			"header", hdr,
		)
		return errIncompatibleHeader
	}

	// Wait for the correct block to arrive.
	n.transitionLocked(StateWaitingForBlock{
		ioRoot:           ioRoot,
		batch:            batch,
		batchSpanCtx:     batchSpanCtx,
		header:           &hdr,
		txnSchedSig:      txnSchedSig,
		inputStorageSigs: inputStorageSigs,
	})

	return nil
}

func (n *Node) worker() {
	defer close(n.quitCh)
	defer (n.cancelCtx)()

	// Wait for the common node to be initialized.
	select {
	case <-n.commonNode.Initialized():
	case <-n.stopCh:
		close(n.initCh)
		return
	}

	n.logger.Info("starting committee node")

	// Provision the hosted runtime.
	hrt, hrtNotifier, err := n.ProvisionHostedRuntime(n.ctx)
	if err != nil {
		n.logger.Error("failed to provision hosted runtime",
			"err", err,
		)
		return
	}

	hrtEventCh, hrtSub, err := hrt.WatchEvents(n.ctx)
	if err != nil {
		n.logger.Error("failed to subscribe to hosted runtime events",
			"err", err,
		)
		return
	}
	defer hrtSub.Close()

	if err = hrt.Start(); err != nil {
		n.logger.Error("failed to start hosted runtime",
			"err", err,
		)
		return
	}
	defer hrt.Stop()

	if err = hrtNotifier.Start(); err != nil {
		n.logger.Error("failed to start runtime notifier",
			"err", err,
		)
		return
	}
	defer hrtNotifier.Stop()

	// We are initialized.
	close(n.initCh)

	var runtimeVersion version.Version
	for {
		// Check if we are currently processing a batch. In this case, we also
		// need to select over the result channel.
		var processingDoneCh chan *protocol.ComputedBatch

		func() {
			n.commonNode.CrossNode.Lock()
			defer n.commonNode.CrossNode.Unlock()

			if stateProcessing, ok := n.state.(StateProcessingBatch); ok {
				processingDoneCh = stateProcessing.done
			}
		}()

		select {
		case <-n.stopCh:
			n.logger.Info("termination requested")
			return
		case ev := <-hrtEventCh:
			switch {
			case ev.Started != nil:
				// We are now able to service requests for this runtime.
				runtimeVersion = ev.Started.Version

				n.roleProvider.SetAvailable(func(nd *node.Node) error {
					rt := nd.AddOrUpdateRuntime(n.commonNode.Runtime.ID())
					rt.Version = runtimeVersion
					rt.Capabilities.TEE = ev.Started.CapabilityTEE
					return nil
				})
			case ev.Updated != nil:
				// Update runtime capabilities.
				n.roleProvider.SetAvailable(func(nd *node.Node) error {
					rt := nd.AddOrUpdateRuntime(n.commonNode.Runtime.ID())
					rt.Version = runtimeVersion
					rt.Capabilities.TEE = ev.Updated.CapabilityTEE
					return nil
				})
			case ev.FailedToStart != nil, ev.Stopped != nil:
				// Runtime failed to start or was stopped -- we can no longer service requests.
				n.roleProvider.SetUnavailable()
			default:
				// Unknown event.
				n.logger.Warn("unknown worker event",
					"ev", ev,
				)
			}
		case batch := <-processingDoneCh:
			// Batch processing has finished.
			if batch == nil {
				n.logger.Warn("worker has aborted batch processing")
				func() {
					n.commonNode.CrossNode.Lock()
					defer n.commonNode.CrossNode.Unlock()

					// To avoid stale events, check if the stored state is still valid.
					if state, ok := n.state.(StateProcessingBatch); !ok || state.done != processingDoneCh {
						return
					}
					n.abortBatchLocked(errRuntimeAborted)
				}()
				break
			}

			n.logger.Info("worker has finished processing a batch")

			func() {
				n.commonNode.CrossNode.Lock()
				defer n.commonNode.CrossNode.Unlock()

				// To avoid stale events, check if the stored state is still valid.
				if state, ok := n.state.(StateProcessingBatch); !ok || state.done != processingDoneCh {
					return
				}
				n.proposeBatchLocked(batch)
			}()
		case <-n.reselect:
			// Recalculate select set.
		}
	}
}

func NewNode(
	commonNode *committee.Node,
	mergeNode *mergeCommittee.Node,
	commonCfg commonWorker.Config,
	roleProvider registration.RoleProvider,
) (*Node, error) {
	metricsOnce.Do(func() {
		prometheus.MustRegister(nodeCollectors...)
	})

	// Prepare the runtime host node helpers.
	rhn, err := commonWorker.NewRuntimeHostNode(commonCfg.RuntimeHost, commonNode)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	n := &Node{
		RuntimeHostNode:  rhn,
		commonNode:       commonNode,
		mergeNode:        mergeNode,
		commonCfg:        commonCfg,
		roleProvider:     roleProvider,
		ctx:              ctx,
		cancelCtx:        cancel,
		stopCh:           make(chan struct{}),
		quitCh:           make(chan struct{}),
		initCh:           make(chan struct{}),
		state:            StateNotReady{},
		stateTransitions: pubsub.NewBroker(false),
		reselect:         make(chan struct{}, 1),
		logger:           logging.GetLogger("worker/executor/committee").With("runtime_id", commonNode.Runtime.ID()),
	}

	return n, nil
}
