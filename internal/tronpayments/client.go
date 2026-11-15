package tronpayments

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
	"regexp"
	"strings"
	"time"
)

var (
	ErrPending        = errors.New("TRON transaction is not solidified")
	ErrInvalidReceipt = errors.New("invalid TRON USDT receipt")
	ErrProvider       = errors.New("TRON RPC unavailable")
)

const (
	maxResponseBytes      = 1 << 20
	maxBlockResponseBytes = 8 << 20
	transferTopicHex      = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
)

var txHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

type Receipt struct {
	TxHash          string
	BlockNumber     int64
	BlockTime       time.Time
	LogIndex        int
	FromAddress     string
	ToAddress       string
	ContractAddress string
	AmountBaseUnits int64
}

type Client struct {
	verifyEndpoint   string
	latestEndpoint   string
	blockEndpoint    string
	apiKey           string
	collection       [20]byte
	collectionBase58 string
	contract         [20]byte
	http             *http.Client
}

func NewClient(config Config) (*Client, error) {
	if !validRPCOrigin(config.RPCURL) || config.CollectionAddress == "" {
		return nil, errors.New("incomplete TRON payment configuration")
	}
	if err := ValidateMainnetAddress(config.CollectionAddress); err != nil {
		return nil, err
	}
	collection, _ := decodeBase58Check(config.CollectionAddress)
	contract, err := decodeBase58Check(OfficialUSDTAddress)
	if err != nil {
		return nil, errors.New("invalid compiled TRON USDT contract")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("TRON RPC redirect rejected")
		},
	}
	result := &Client{
		verifyEndpoint:   config.RPCURL + "/walletsolidity/gettransactioninfobyid",
		latestEndpoint:   config.RPCURL + "/walletsolidity/getnowblock",
		blockEndpoint:    config.RPCURL + "/walletsolidity/gettransactioninfobyblocknum",
		apiKey:           config.apiKey,
		collectionBase58: config.CollectionAddress,
		http:             client,
	}
	copy(result.collection[:], collection[1:])
	copy(result.contract[:], contract[1:])
	return result, nil
}

type transactionInfo struct {
	ID             string `json:"id"`
	Result         string `json:"result"`
	BlockNumber    int64  `json:"blockNumber"`
	BlockTimeStamp int64  `json:"blockTimeStamp"`
	Receipt        struct {
		Result string `json:"result"`
	} `json:"receipt"`
	Log []struct {
		Address string   `json:"address"`
		Topics  []string `json:"topics"`
		Data    string   `json:"data"`
	} `json:"log"`
}

func (c *Client) Verify(ctx context.Context, txHash string, expectedBaseUnits int64) (Receipt, error) {
	if c == nil || c.http == nil || !txHashPattern.MatchString(txHash) || expectedBaseUnits <= 0 {
		return Receipt{}, ErrInvalidReceipt
	}
	txHash = strings.ToLower(txHash)
	var info transactionInfo
	if err := c.postJSON(ctx, c.verifyEndpoint, map[string]string{"value": txHash}, maxResponseBytes, &info); err != nil {
		return Receipt{}, err
	}
	if info.ID == "" {
		return Receipt{}, ErrPending
	}
	if strings.ToLower(info.ID) != txHash || !transactionSucceeded(info) || info.BlockNumber <= 0 || info.BlockTimeStamp <= 0 {
		return Receipt{}, ErrInvalidReceipt
	}
	matches := make([]Receipt, 0, 1)
	for index, event := range info.Log {
		receipt, ok := c.matchTransfer(event.Address, event.Topics, event.Data, expectedBaseUnits)
		if !ok {
			continue
		}
		receipt.TxHash = txHash
		receipt.BlockNumber = info.BlockNumber
		receipt.BlockTime = time.UnixMilli(info.BlockTimeStamp).UTC()
		receipt.LogIndex = index
		matches = append(matches, receipt)
	}
	if len(matches) != 1 {
		return Receipt{}, ErrInvalidReceipt
	}
	return matches[0], nil
}

func (c *Client) LatestSolidBlock(ctx context.Context) (int64, error) {
	if c == nil || c.http == nil {
		return 0, ErrProvider
	}
	var block struct {
		Header struct {
			RawData struct {
				Number int64 `json:"number"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := c.postJSON(ctx, c.latestEndpoint, struct{}{}, maxResponseBytes, &block); err != nil {
		return 0, err
	}
	if block.Header.RawData.Number <= 0 {
		return 0, fmt.Errorf("%w: missing solidified height", ErrProvider)
	}
	return block.Header.RawData.Number, nil
}

func (c *Client) ScanSolidBlock(ctx context.Context, height int64) ([]Receipt, error) {
	if c == nil || c.http == nil || height <= 0 {
		return nil, ErrInvalidReceipt
	}
	var infos []transactionInfo
	if err := c.postJSON(ctx, c.blockEndpoint, map[string]int64{"num": height}, maxBlockResponseBytes, &infos); err != nil {
		return nil, err
	}
	receipts := make([]Receipt, 0)
	for _, info := range infos {
		if info.BlockNumber != height {
			return nil, fmt.Errorf("%w: wrong block height", ErrProvider)
		}
		if !transactionSucceeded(info) {
			continue
		}
		matches := make([]Receipt, 0, 1)
		for index, event := range info.Log {
			receipt, ok := c.matchTransfer(event.Address, event.Topics, event.Data, 0)
			if !ok {
				continue
			}
			receipt.LogIndex = index
			matches = append(matches, receipt)
		}
		if len(matches) != 1 {
			continue
		}
		txHash := strings.ToLower(info.ID)
		if !txHashPattern.MatchString(txHash) || info.BlockTimeStamp <= 0 {
			return nil, fmt.Errorf("%w: malformed matching transaction", ErrProvider)
		}
		receipt := matches[0]
		receipt.TxHash = txHash
		receipt.BlockNumber = info.BlockNumber
		receipt.BlockTime = time.UnixMilli(info.BlockTimeStamp).UTC()
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func transactionSucceeded(info transactionInfo) bool {
	if info.Receipt.Result != "SUCCESS" {
		return false
	}
	switch info.Result {
	case "", "SUCESS", "SUCCESS":
		return true
	default:
		return false
	}
}

func (c *Client) postJSON(ctx context.Context, endpoint string, input any, limit int64, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("%w: request", ErrProvider)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: request", ErrProvider)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		request.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return fmt.Errorf("%w: request failed", ErrProvider)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP status %d", ErrProvider, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return fmt.Errorf("%w: invalid response body", ErrProvider)
	}
	if err = json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("%w: invalid JSON", ErrProvider)
	}
	return nil
}

func (c *Client) matchTransfer(contractHex string, topics []string, data string, expected int64) (Receipt, bool) {
	contract, err := exactHex(contractHex, 20)
	if err != nil || !bytes.Equal(contract, c.contract[:]) || len(topics) != 3 || strings.ToLower(topics[0]) != transferTopicHex {
		return Receipt{}, false
	}
	fromTopic, err := exactHex(topics[1], 32)
	if err != nil {
		return Receipt{}, false
	}
	toTopic, err := exactHex(topics[2], 32)
	if err != nil || !bytes.Equal(toTopic[12:], c.collection[:]) {
		return Receipt{}, false
	}
	amountBytes, err := exactHex(data, 32)
	if err != nil {
		return Receipt{}, false
	}
	amount := new(big.Int).SetBytes(amountBytes)
	if !amount.IsInt64() || amount.Sign() <= 0 || expected > 0 && amount.Int64() != expected {
		return Receipt{}, false
	}
	fromPayload := append([]byte{0x41}, fromTopic[12:]...)
	return Receipt{
		FromAddress:     encodeBase58Check(fromPayload),
		ToAddress:       c.collectionBase58,
		ContractAddress: OfficialUSDTAddress,
		AmountBaseUnits: amount.Int64(),
	}, true
}

func exactHex(value string, bytes int) ([]byte, error) {
	if len(value) != bytes*2 {
		return nil, errors.New("wrong hex length")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}
