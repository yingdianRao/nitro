package arbnode

import (
	"context"
	"errors"
	"sync/atomic"

	flag "github.com/spf13/pflag"
	"github.com/yingdianRao/nitro/arbutil"
	"github.com/yingdianRao/nitro/execution"
)

type SyncMonitor struct {
	config      *SyncMonitorConfig
	inboxReader *InboxReader
	txStreamer  *TransactionStreamer
	coordinator *SeqCoordinator
	exec        execution.FullExecutionClient
	initialized bool
}

func NewSyncMonitor(config *SyncMonitorConfig) *SyncMonitor {
	return &SyncMonitor{
		config: config,
	}
}

type SyncMonitorConfig struct {
	BlockBuildLag                       uint64 `koanf:"block-build-lag"`
	BlockBuildSequencerInboxLag         uint64 `koanf:"block-build-sequencer-inbox-lag"`
	CoordinatorMsgLag                   uint64 `koanf:"coordinator-msg-lag"`
	SafeBlockWaitForBlockValidator      bool   `koanf:"safe-block-wait-for-block-validator"`
	FinalizedBlockWaitForBlockValidator bool   `koanf:"finalized-block-wait-for-block-validator"`
}

var DefaultSyncMonitorConfig = SyncMonitorConfig{
	BlockBuildLag:                       20,
	BlockBuildSequencerInboxLag:         0,
	CoordinatorMsgLag:                   15,
	SafeBlockWaitForBlockValidator:      false,
	FinalizedBlockWaitForBlockValidator: false,
}

func SyncMonitorConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Uint64(prefix+".block-build-lag", DefaultSyncMonitorConfig.BlockBuildLag, "allowed lag between messages read and blocks built")
	f.Uint64(prefix+".block-build-sequencer-inbox-lag", DefaultSyncMonitorConfig.BlockBuildSequencerInboxLag, "allowed lag between messages read from sequencer inbox and blocks built")
	f.Uint64(prefix+".coordinator-msg-lag", DefaultSyncMonitorConfig.CoordinatorMsgLag, "allowed lag between local and remote messages")
	f.Bool(prefix+".safe-block-wait-for-block-validator", DefaultSyncMonitorConfig.SafeBlockWaitForBlockValidator, "wait for block validator to complete before returning safe block number")
	f.Bool(prefix+".finalized-block-wait-for-block-validator", DefaultSyncMonitorConfig.FinalizedBlockWaitForBlockValidator, "wait for block validator to complete before returning finalized block number")
}

func (s *SyncMonitor) Initialize(inboxReader *InboxReader, txStreamer *TransactionStreamer, coordinator *SeqCoordinator, exec execution.FullExecutionClient) {
	s.inboxReader = inboxReader
	s.txStreamer = txStreamer
	s.coordinator = coordinator
	s.exec = exec
	s.initialized = true
}

func (s *SyncMonitor) SyncProgressMap() map[string]interface{} {
	syncing := false
	res := make(map[string]interface{})

	if !s.initialized {
		res["err"] = "uninitialized"
		return res
	}

	broadcasterQueuedMessagesPos := atomic.LoadUint64(&(s.txStreamer.broadcasterQueuedMessagesPos))

	if broadcasterQueuedMessagesPos != 0 { // unprocessed feed
		syncing = true
	}
	res["broadcasterQueuedMessagesPos"] = broadcasterQueuedMessagesPos

	builtMessageCount, err := s.exec.HeadMessageNumber()
	if err != nil {
		res["builtMessageCountError"] = err.Error()
		syncing = true
		builtMessageCount = 0
	} else {
		blockNum := s.exec.MessageIndexToBlockNumber(builtMessageCount)
		res["blockNum"] = blockNum
		builtMessageCount++
		res["messageOfLastBlock"] = builtMessageCount
	}

	msgCount, err := s.txStreamer.GetMessageCount()
	if err != nil {
		res["msgCountError"] = err.Error()
		syncing = true
	} else {
		res["msgCount"] = msgCount
		if builtMessageCount+arbutil.MessageIndex(s.config.BlockBuildLag) < msgCount {
			syncing = true
		}
	}

	if s.inboxReader != nil {
		batchSeen := s.inboxReader.GetLastSeenBatchCount()
		_, batchProcessed := s.inboxReader.GetLastReadBlockAndBatchCount()

		if (batchSeen == 0) || // error or not yet read inbox
			(batchProcessed < batchSeen) { // unprocessed inbox messages
			syncing = true
		}
		res["batchSeen"] = batchSeen
		res["batchProcessed"] = batchProcessed

		processedMetadata, err := s.inboxReader.Tracker().GetBatchMetadata(batchProcessed - 1)
		if err != nil {
			res["batchMetadataError"] = err.Error()
			syncing = true
		} else {
			res["messageOfProcessedBatch"] = processedMetadata.MessageCount
			if builtMessageCount+arbutil.MessageIndex(s.config.BlockBuildSequencerInboxLag) < processedMetadata.MessageCount {
				syncing = true
			}
		}

		l1reader := s.inboxReader.l1Reader
		if l1reader != nil {
			header, err := l1reader.LastHeaderWithError()
			if err != nil {
				res["lastL1HeaderErr"] = err
			}
			if header != nil {
				res["lastL1BlockNum"] = header.Number
				res["lastl1BlockHash"] = header.Hash()
			}
		}
	}

	if s.coordinator != nil {
		coordinatorMessageCount, err := s.coordinator.GetRemoteMsgCount() //NOTE: this creates a remote call
		if err != nil {
			res["coordinatorMsgCountError"] = err.Error()
			syncing = true
		} else {
			res["coordinatorMessageCount"] = coordinatorMessageCount
			if msgCount+arbutil.MessageIndex(s.config.CoordinatorMsgLag) < coordinatorMessageCount {
				syncing = true
			}
		}
	}

	if !syncing {
		return make(map[string]interface{})
	}

	return res
}

func (s *SyncMonitor) SafeBlockNumber(ctx context.Context) (uint64, error) {
	if s.inboxReader == nil || !s.initialized {
		return 0, errors.New("not set up for safeblock")
	}
	msg, err := s.inboxReader.GetSafeMsgCount(ctx)
	if err != nil {
		return 0, err
	}
	// If SafeBlockWaitForBlockValidator is true, we want to wait for the block validator to finish
	if s.config.SafeBlockWaitForBlockValidator {
		latestValidatedCount, err := s.getLatestValidatedCount()
		if err != nil {
			return 0, err
		}
		if msg > latestValidatedCount {
			msg = latestValidatedCount
		}
	}
	block := s.exec.MessageIndexToBlockNumber(msg - 1)
	return block, nil
}

func (s *SyncMonitor) getLatestValidatedCount() (arbutil.MessageIndex, error) {
	if s.txStreamer.validator == nil {
		return 0, errors.New("validator not set up")
	}
	return s.txStreamer.validator.GetValidated(), nil
}

func (s *SyncMonitor) FinalizedBlockNumber(ctx context.Context) (uint64, error) {
	if s.inboxReader == nil || !s.initialized {
		return 0, errors.New("not set up for safeblock")
	}
	msg, err := s.inboxReader.GetFinalizedMsgCount(ctx)
	if err != nil {
		return 0, err
	}
	// If FinalizedBlockWaitForBlockValidator is true, we want to wait for the block validator to finish
	if s.config.FinalizedBlockWaitForBlockValidator {
		latestValidatedCount, err := s.getLatestValidatedCount()
		if err != nil {
			return 0, err
		}
		if msg > latestValidatedCount {
			msg = latestValidatedCount
		}
	}
	block := s.exec.MessageIndexToBlockNumber(msg - 1)
	return block, nil
}

func (s *SyncMonitor) Synced() bool {
	return len(s.SyncProgressMap()) == 0
}
