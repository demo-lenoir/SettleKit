package indexer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const rpcTimeout = 5 * time.Second

type Log struct {
	Address     common.Address
	Topics      []common.Hash
	Data        []byte
	BlockHash   common.Hash
	BlockNumber uint64
	TxHash      common.Hash
	Index       uint
	Removed     bool
}

type Block struct {
	Number uint64
	Hash   common.Hash
	Parent common.Hash
	Time   time.Time
	Logs   []Log
}

type Source interface {
	ChainID(context.Context) (*big.Int, error)
	Head(context.Context) (uint64, error)
	Block(context.Context, uint64) (Block, error)
}

type RPCSource struct {
	client   *http.Client
	url      string
	contract common.Address
}

func DialRPC(ctx context.Context, rawURL string, contract common.Address) (*RPCSource, error) {
	source, err := NewRPCSource(rawURL, contract)
	if err != nil {
		return nil, err
	}
	if _, err := source.ChainID(ctx); err != nil {
		return nil, fmt.Errorf("connect RPC: %w", err)
	}
	return source, nil
}

func NewRPCSource(rawURL string, contract common.Address) (*RPCSource, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || contract == (common.Address{}) {
		return nil, errors.New("invalid RPC URL or escrow address")
	}
	return &RPCSource{client: &http.Client{Timeout: rpcTimeout}, url: rawURL, contract: contract}, nil
}

func (s *RPCSource) Close() { s.client.CloseIdleConnections() }

func (s *RPCSource) rpc(ctx context.Context, method string, params []any, output any) error {
	bounded, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{"2.0", 1, method, params})
	if err != nil {
		return fmt.Errorf("encode RPC %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(bounded, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("construct RPC %s request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("RPC %s transport failed", method)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("RPC %s HTTP status %d", method, resp.StatusCode)
	}
	var envelope struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("decode RPC %s response: %w", method, err)
	}
	if envelope.ID != 1 || envelope.Error != nil || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return fmt.Errorf("RPC %s returned error or empty result", method)
	}
	if err := json.Unmarshal(envelope.Result, output); err != nil {
		return fmt.Errorf("decode RPC %s result: %w", method, err)
	}
	return nil
}

func hexUint(raw string) (uint64, error) {
	if !strings.HasPrefix(raw, "0x") || len(raw) < 3 {
		return 0, errors.New("invalid hex quantity")
	}
	return strconv.ParseUint(raw[2:], 16, 64)
}

func (s *RPCSource) ChainID(ctx context.Context) (*big.Int, error) {
	var raw string
	if err := s.rpc(ctx, "eth_chainId", []any{}, &raw); err != nil {
		return nil, err
	}
	value, err := hexUint(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid RPC chain ID: %w", err)
	}
	return new(big.Int).SetUint64(value), nil
}

func (s *RPCSource) Head(ctx context.Context) (uint64, error) {
	var raw string
	if err := s.rpc(ctx, "eth_blockNumber", []any{}, &raw); err != nil {
		return 0, err
	}
	return hexUint(raw)
}

func (s *RPCSource) Block(ctx context.Context, number uint64) (Block, error) {
	var header struct {
		Number    string `json:"number"`
		Hash      string `json:"hash"`
		Parent    string `json:"parentHash"`
		Timestamp string `json:"timestamp"`
	}
	if err := s.rpc(ctx, "eth_getBlockByNumber", []any{fmt.Sprintf("0x%x", number), false}, &header); err != nil {
		return Block{}, err
	}
	blockNumber, err := hexUint(header.Number)
	if err != nil || blockNumber != number || !validHash(header.Hash) || !validHash(header.Parent) {
		return Block{}, errors.New("inconsistent RPC block identity")
	}
	stamp, err := hexUint(header.Timestamp)
	if err != nil || stamp > uint64(^uint64(0)>>1) {
		return Block{}, errors.New("invalid RPC block timestamp")
	}
	block := Block{Number: number, Hash: common.HexToHash(header.Hash), Parent: common.HexToHash(header.Parent), Time: time.Unix(int64(stamp), 0).UTC()}
	var rawLogs []struct {
		Address         string   `json:"address"`
		Topics          []string `json:"topics"`
		Data            string   `json:"data"`
		BlockHash       string   `json:"blockHash"`
		BlockNumber     string   `json:"blockNumber"`
		TransactionHash string   `json:"transactionHash"`
		LogIndex        string   `json:"logIndex"`
		Removed         bool     `json:"removed"`
	}
	if err := s.rpc(ctx, "eth_getLogs", []any{map[string]any{"blockHash": block.Hash.Hex(), "address": s.contract.Hex()}}, &rawLogs); err != nil {
		return Block{}, err
	}
	if len(rawLogs) > 1000 {
		return Block{}, errors.New("more than 1000 escrow logs in block")
	}
	for _, raw := range rawLogs {
		logNumber, numberErr := hexUint(raw.BlockNumber)
		index, indexErr := hexUint(raw.LogIndex)
		if numberErr != nil || indexErr != nil || index > 2147483647 || logNumber != number || !common.IsHexAddress(raw.Address) || !validHash(raw.BlockHash) || !validHash(raw.TransactionHash) || common.HexToHash(raw.BlockHash) != block.Hash || common.HexToAddress(raw.Address) != s.contract || raw.Removed {
			return Block{}, errors.New("inconsistent RPC log identity")
		}
		if len(raw.Topics) > 4 {
			return Block{}, errors.New("too many RPC log topics")
		}
		log := Log{Address: s.contract, BlockHash: block.Hash, BlockNumber: number, TxHash: common.HexToHash(raw.TransactionHash), Index: uint(index)}
		for _, topic := range raw.Topics {
			if !validHash(topic) {
				return Block{}, errors.New("invalid RPC log topic")
			}
			log.Topics = append(log.Topics, common.HexToHash(topic))
		}
		if !strings.HasPrefix(raw.Data, "0x") || len(raw.Data) > 8194 {
			return Block{}, errors.New("invalid RPC log data")
		}
		data, err := hex.DecodeString(raw.Data[2:])
		if err != nil {
			return Block{}, errors.New("invalid RPC log data hex")
		}
		log.Data = data
		block.Logs = append(block.Logs, log)
	}
	return block, nil
}

func validHash(raw string) bool {
	if len(raw) != 66 || !strings.HasPrefix(raw, "0x") {
		return false
	}
	for _, r := range raw[2:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
