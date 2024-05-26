// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/yingdianRao/nitro/arbos"
	"github.com/yingdianRao/nitro/arbos/arbosState"
	"github.com/yingdianRao/nitro/arbos/arbostypes"
	"github.com/yingdianRao/nitro/arbos/burn"
	"github.com/yingdianRao/nitro/arbstate"
	"github.com/yingdianRao/nitro/arbutil"
	"github.com/yingdianRao/nitro/cmd/chaininfo"
	"github.com/yingdianRao/nitro/das/celestia"
	"github.com/yingdianRao/nitro/das/celestia/tree"
	"github.com/yingdianRao/nitro/das/dastree"
	"github.com/yingdianRao/nitro/gethhook"
	"github.com/yingdianRao/nitro/wavmio"
)

func getBlockHeaderByHash(hash common.Hash) *types.Header {
	enc, err := wavmio.ResolveTypedPreimage(arbutil.Keccak256PreimageType, hash)
	if err != nil {
		panic(fmt.Errorf("Error resolving preimage: %w", err))
	}
	header := &types.Header{}
	err = rlp.DecodeBytes(enc, &header)
	if err != nil {
		panic(fmt.Errorf("Error parsing resolved block header: %w", err))
	}
	return header
}

type WavmChainContext struct{}

func (c WavmChainContext) Engine() consensus.Engine {
	return arbos.Engine{}
}

func (c WavmChainContext) GetHeader(hash common.Hash, num uint64) *types.Header {
	header := getBlockHeaderByHash(hash)
	if !header.Number.IsUint64() || header.Number.Uint64() != num {
		panic(fmt.Sprintf("Retrieved wrong block number for header hash %v -- requested %v but got %v", hash, num, header.Number.String()))
	}
	return header
}

type WavmInbox struct{}

func (i WavmInbox) PeekSequencerInbox() ([]byte, common.Hash, error) {
	pos := wavmio.GetInboxPosition()
	res := wavmio.ReadInboxMessage(pos)
	log.Info("PeekSequencerInbox", "pos", pos, "res[:8]", res[:8])
	// Our BlobPreimageReader doesn't need the block hash
	return res, common.Hash{}, nil
}

func (i WavmInbox) GetSequencerInboxPosition() uint64 {
	pos := wavmio.GetInboxPosition()
	log.Info("GetSequencerInboxPosition", "pos", pos)
	return pos
}

func (i WavmInbox) AdvanceSequencerInbox() {
	log.Info("AdvanceSequencerInbox")
	wavmio.AdvanceInboxMessage()
}

func (i WavmInbox) GetPositionWithinMessage() uint64 {
	pos := wavmio.GetPositionWithinMessage()
	log.Info("GetPositionWithinMessage", "pos", pos)
	return pos
}

func (i WavmInbox) SetPositionWithinMessage(pos uint64) {
	log.Info("SetPositionWithinMessage", "pos", pos)
	wavmio.SetPositionWithinMessage(pos)
}

func (i WavmInbox) ReadDelayedInbox(seqNum uint64) (*arbostypes.L1IncomingMessage, error) {
	log.Info("ReadDelayedMsg", "seqNum", seqNum)
	data := wavmio.ReadDelayedInboxMessage(seqNum)
	return arbostypes.ParseIncomingL1Message(bytes.NewReader(data), func(batchNum uint64) ([]byte, error) {
		return wavmio.ReadInboxMessage(batchNum), nil
	})
}

type PreimageDASReader struct {
}

func (dasReader *PreimageDASReader) GetByHash(ctx context.Context, hash common.Hash) ([]byte, error) {
	oracle := func(hash common.Hash) ([]byte, error) {
		return wavmio.ResolveTypedPreimage(arbutil.Keccak256PreimageType, hash)
	}
	return dastree.Content(hash, oracle)
}

func (dasReader *PreimageDASReader) HealthCheck(ctx context.Context) error {
	return nil
}

func (dasReader *PreimageDASReader) ExpirationPolicy(ctx context.Context) (arbstate.ExpirationPolicy, error) {
	return arbstate.DiscardImmediately, nil
}

type BlobPreimageReader struct {
}

func (r *BlobPreimageReader) GetBlobs(
	ctx context.Context,
	batchBlockHash common.Hash,
	versionedHashes []common.Hash,
) ([]kzg4844.Blob, error) {
	var blobs []kzg4844.Blob
	for _, h := range versionedHashes {
		var blob kzg4844.Blob
		preimage, err := wavmio.ResolveTypedPreimage(arbutil.EthVersionedHashPreimageType, h)
		if err != nil {
			return nil, err
		}
		if len(preimage) != len(blob) {
			return nil, fmt.Errorf("for blob %v got back preimage of length %v but expected blob length %v", h, len(preimage), len(blob))
		}
		copy(blob[:], preimage)
		blobs = append(blobs, blob)
	}
	return blobs, nil
}

func (r *BlobPreimageReader) Initialize(ctx context.Context) error {
	return nil
}

type PreimageCelestiaReader struct {
}

func (dasReader *PreimageCelestiaReader) Read(ctx context.Context, blobPointer *celestia.BlobPointer) ([]byte, *celestia.SquareData, error) {
	oracle := func(hash common.Hash) ([]byte, error) {
		return wavmio.ResolveTypedPreimage(arbutil.Sha2_256PreimageType, hash)
	}

	// first, walk down the merkle tree
	leaves, err := tree.MerkleTreeContent(oracle, common.BytesToHash(blobPointer.DataRoot[:]))
	if err != nil {
		log.Warn("Error revealing contents behind data root", "err", err)
		return nil, nil, err
	}

	squareSize := uint64(len(leaves)) / 2
	// split leaves in half to get row roots
	rowRoots := leaves[:squareSize]
	// We get the original data square size, wich is (size_of_the_extended_square / 2)
	odsSize := squareSize / 2

	startRow := blobPointer.Start / squareSize

	startIndex := blobPointer.Start % squareSize

	if startIndex > odsSize {
		return nil, nil, fmt.Errorf("Error getting number of shares in first row: StartIndex %v > OdsSize %v", startIndex, odsSize)
	}
	firtsRowShares := odsSize - startIndex

	if blobPointer.SharesLength == 0 {
		return nil, nil, fmt.Errorf("Error, shares length is %v", blobPointer.SharesLength)
	}

	var endIndex uint64
	var endRow uint64
	var remainingShares uint64
	var rowsNeeded uint64

	if blobPointer.SharesLength <= firtsRowShares {
		endIndex = blobPointer.Start + blobPointer.SharesLength - 1
		endRow = startRow
	} else {
		if firtsRowShares > blobPointer.SharesLength {
			return nil, nil, fmt.Errorf("Error, shares in first row %v is larger than total share length %v", firtsRowShares, blobPointer.SharesLength)
		}
		remainingShares = blobPointer.SharesLength - firtsRowShares
		rowsNeeded = remainingShares / odsSize
		endRow = startRow + rowsNeeded + func() uint64 {
			if remainingShares%odsSize > 0 {
				return 1
			} else {
				return 0
			}
		}()
		partialRow := func() bool {
			if blobPointer.SharesLength%squareSize > 0 {
				return true
			} else {
				return false
			}
		}()

		if partialRow {
			if remainingShares%odsSize < 1 {
				return nil, nil, fmt.Errorf("Error calculating index for partial row remainingShares mod odsSize is %v, which is less than 1", remainingShares%odsSize)
			}
			endIndex = endRow*odsSize + (remainingShares%odsSize - 1)
		} else {
			if (endRow * odsSize) < 1 {
				return nil, nil, fmt.Errorf("Error, endRow * odszie is %v, which is less than 1", endRow*odsSize)
			}
			endIndex = (endRow * odsSize) - 1
		}
	}
	endIndex = endIndex % squareSize

	if startIndex > odsSize {
		return nil, nil, fmt.Errorf("Error getting content, start index %v is larger than odsSize %v", startIndex, odsSize)
	}

	if endIndex+1 > odsSize {
		return nil, nil, fmt.Errorf("Error getting content, end index %v is larger than odsSize %v", endIndex, odsSize)
	}
	// get rows behind row root and shares for our blob
	rows := [][][]byte{}
	shares := [][]byte{}
	for i := startRow; i <= endRow; i++ {
		row, err := tree.NmtContent(oracle, rowRoots[i])
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, row)

		if odsSize > uint64(len(row)) {
			return nil, nil, fmt.Errorf("Error getting shares, odsSize %v is largern than row length %v", odsSize, len(row))
		}
		odsRow := row[:odsSize]

		if startRow == endRow {
			if startIndex > endIndex+1 {
				return nil, nil, fmt.Errorf("Error getting content, start index %v is larger than endIndex %v + 1", startIndex, endIndex)
			}
			shares = append(shares, odsRow[startIndex:endIndex+1]...)
			break
		} else if i == startRow {
			shares = append(shares, odsRow[startIndex:]...)
		} else if i == endRow {
			shares = append(shares, odsRow[:endIndex+1]...)
		} else {
			shares = append(shares, odsRow...)
		}
	}

	data := []byte{}
	if tree.NamespaceSize*2+1 > uint64(len(shares[0])) || tree.NamespaceSize*2+5 > uint64(len(shares[0])) {
		return nil, nil, fmt.Errorf("Error getting sequence length on share of size", len(shares[0]))
	}
	sequenceLength := binary.BigEndian.Uint32(shares[0][tree.NamespaceSize*2+1 : tree.NamespaceSize*2+5])
	for i, share := range shares {
		// trim extra namespace
		share := share[29:]
		if i == 0 {
			data = append(data, share[tree.NamespaceSize+5:]...)
			continue
		}
		data = append(data, share[tree.NamespaceSize+1:]...)
	}
	if sequenceLength > uint32(len(data)) {
		return nil, nil, fmt.Errorf("Error getting blob from shares, sequenceLength %v is larger than length of data %v", sequenceLength, len(data))
	}
	data = data[:sequenceLength]
	squareData := celestia.SquareData{
		RowRoots:    rowRoots,
		ColumnRoots: leaves[squareSize:],
		Rows:        rows,
		SquareSize:  squareSize,
		StartRow:    startRow,
		EndRow:      endRow,
	}
	return data, &squareData, nil
}

// To generate:
// key, _ := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000001")
// sig, _ := crypto.Sign(make([]byte, 32), key)
// println(hex.EncodeToString(sig))
const sampleSignature = "a0b37f8fba683cc68f6574cd43b39f0343a50008bf6ccea9d13231d9e7e2e1e411edc8d307254296264aebfc3dc76cd8b668373a072fd64665b50000e9fcce5201"

// We call this early to populate the secp256k1 ecc basepoint cache in the cached early machine state.
// That means we don't need to re-compute it for every block.
func populateEcdsaCaches() {
	signature, err := hex.DecodeString(sampleSignature)
	if err != nil {
		log.Warn("failed to decode sample signature to populate ECDSA cache", "err", err)
		return
	}
	_, err = crypto.Ecrecover(make([]byte, 32), signature)
	if err != nil {
		log.Warn("failed to recover signature to populate ECDSA cache", "err", err)
		return
	}
}

func main() {
	wavmio.StubInit()
	gethhook.RequireHookedGeth()

	glogger := log.NewGlogHandler(log.StreamHandler(os.Stderr, log.TerminalFormat(false)))
	glogger.Verbosity(log.LvlError)
	log.Root().SetHandler(glogger)

	populateEcdsaCaches()

	raw := rawdb.NewDatabase(PreimageDb{})
	db := state.NewDatabase(raw)

	lastBlockHash := wavmio.GetLastBlockHash()

	var lastBlockHeader *types.Header
	var lastBlockStateRoot common.Hash
	if lastBlockHash != (common.Hash{}) {
		lastBlockHeader = getBlockHeaderByHash(lastBlockHash)
		lastBlockStateRoot = lastBlockHeader.Root
	}

	log.Info("Initial State", "lastBlockHash", lastBlockHash, "lastBlockStateRoot", lastBlockStateRoot)
	statedb, err := state.NewDeterministic(lastBlockStateRoot, db)
	if err != nil {
		panic(fmt.Sprintf("Error opening state db: %v", err.Error()))
	}

	readMessage := func(arbChainParams params.ArbitrumChainParams) *arbostypes.MessageWithMetadata {
		var delayedMessagesRead uint64
		if lastBlockHeader != nil {
			delayedMessagesRead = lastBlockHeader.Nonce.Uint64()
		}

		if arbChainParams.DataAvailabilityCommittee && arbChainParams.CelestiaDA {
			panic(fmt.Sprintf("Error Multiple DA providers enabled: DAC is %v and CelestiaDA is %v", arbChainParams.DataAvailabilityCommittee, arbChainParams.CelestiaDA))
		}

		var dasReader arbstate.DataAvailabilityReader
		if arbChainParams.DataAvailabilityCommittee {
			dasReader = &PreimageDASReader{}
		}
		var celestiaReader celestia.DataAvailabilityReader
		if arbChainParams.CelestiaDA {
			celestiaReader = &PreimageCelestiaReader{}
		}
		backend := WavmInbox{}
		var keysetValidationMode = arbstate.KeysetPanicIfInvalid
		if backend.GetPositionWithinMessage() > 0 {
			keysetValidationMode = arbstate.KeysetDontValidate
		}
		var daProviders []arbstate.DataAvailabilityProvider
		if dasReader != nil {
			daProviders = append(daProviders, arbstate.NewDAProviderDAS(dasReader))
		}
		if celestiaReader != nil {
			daProviders = append(daProviders, arbstate.NewDAProviderCelestia(celestiaReader))
		}
		daProviders = append(daProviders, arbstate.NewDAProviderBlobReader(&BlobPreimageReader{}))
		inboxMultiplexer := arbstate.NewInboxMultiplexer(backend, delayedMessagesRead, daProviders, keysetValidationMode)
		ctx := context.Background()
		message, err := inboxMultiplexer.Pop(ctx)
		if err != nil {
			panic(fmt.Sprintf("Error reading from inbox multiplexer: %v", err.Error()))
		}

		return message
	}

	var newBlock *types.Block
	if lastBlockStateRoot != (common.Hash{}) {
		// ArbOS has already been initialized.
		// Load the chain config and then produce a block normally.

		initialArbosState, err := arbosState.OpenSystemArbosState(statedb, nil, true)
		if err != nil {
			panic(fmt.Sprintf("Error opening initial ArbOS state: %v", err.Error()))
		}
		chainId, err := initialArbosState.ChainId()
		if err != nil {
			panic(fmt.Sprintf("Error getting chain ID from initial ArbOS state: %v", err.Error()))
		}
		genesisBlockNum, err := initialArbosState.GenesisBlockNum()
		if err != nil {
			panic(fmt.Sprintf("Error getting genesis block number from initial ArbOS state: %v", err.Error()))
		}
		chainConfigJson, err := initialArbosState.ChainConfig()
		if err != nil {
			panic(fmt.Sprintf("Error getting chain config from initial ArbOS state: %v", err.Error()))
		}
		var chainConfig *params.ChainConfig
		if len(chainConfigJson) > 0 {
			chainConfig = &params.ChainConfig{}
			err = json.Unmarshal(chainConfigJson, chainConfig)
			if err != nil {
				panic(fmt.Sprintf("Error parsing chain config: %v", err.Error()))
			}
			if chainConfig.ChainID.Cmp(chainId) != 0 {
				panic(fmt.Sprintf("Error: chain id mismatch, chainID: %v, chainConfig.ChainID: %v", chainId, chainConfig.ChainID))
			}
			if chainConfig.ArbitrumChainParams.GenesisBlockNum != genesisBlockNum {
				panic(fmt.Sprintf("Error: genesis block number mismatch, genesisBlockNum: %v, chainConfig.ArbitrumParams.GenesisBlockNum: %v", genesisBlockNum, chainConfig.ArbitrumChainParams.GenesisBlockNum))
			}
		} else {
			log.Info("Falling back to hardcoded chain config.")
			chainConfig, err = chaininfo.GetChainConfig(chainId, "", genesisBlockNum, []string{}, "")
			if err != nil {
				panic(err)
			}
		}

		// need to add Celestia or just "ExternalDA" as an option to the ArbitrumChainParams
		// for now we hard code Cthis to treu and hardcode Celestia in `readMessage`
		// to test the integration
		message := readMessage(chainConfig.ArbitrumChainParams)

		chainContext := WavmChainContext{}
		batchFetcher := func(batchNum uint64) ([]byte, error) {
			return wavmio.ReadInboxMessage(batchNum), nil
		}
		newBlock, _, err = arbos.ProduceBlock(message.Message, message.DelayedMessagesRead, lastBlockHeader, statedb, chainContext, chainConfig, batchFetcher)
		if err != nil {
			panic(err)
		}

	} else {
		// Initialize ArbOS with this init message and create the genesis block.

		message := readMessage(params.ArbitrumChainParams{})

		initMessage, err := message.Message.ParseInitMessage()
		if err != nil {
			panic(err)
		}
		chainConfig := initMessage.ChainConfig
		if chainConfig == nil {
			log.Info("No chain config in the init message. Falling back to hardcoded chain config.")
			chainConfig, err = chaininfo.GetChainConfig(initMessage.ChainId, "", 0, []string{}, "")
			if err != nil {
				panic(err)
			}
		}

		_, err = arbosState.InitializeArbosState(statedb, burn.NewSystemBurner(nil, false), chainConfig, initMessage)
		if err != nil {
			panic(fmt.Sprintf("Error initializing ArbOS: %v", err.Error()))
		}

		newBlock = arbosState.MakeGenesisBlock(common.Hash{}, 0, 0, statedb.IntermediateRoot(true), chainConfig)

	}

	newBlockHash := newBlock.Hash()

	log.Info("Final State", "newBlockHash", newBlockHash, "StateRoot", newBlock.Root())

	extraInfo := types.DeserializeHeaderExtraInformation(newBlock.Header())
	if extraInfo.ArbOSFormatVersion == 0 {
		panic(fmt.Sprintf("Error deserializing header extra info: %+v", newBlock.Header()))
	}
	wavmio.SetLastBlockHash(newBlockHash)
	wavmio.SetSendRoot(extraInfo.SendRoot)

	wavmio.StubFinal()
}