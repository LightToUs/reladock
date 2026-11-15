package tronpayments

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testCollectionAddress = "TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw"

func clearTronEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"TRON_USDT_RPC_URL", "TRON_USDT_COLLECTION_ADDRESS", "TRON_API_KEY"} {
		t.Setenv(key, "")
	}
}

func TestConfigDisabledWhenAllValuesAreAbsent(t *testing.T) {
	clearTronEnv(t)
	config, enabled, err := ConfigFromEnv()
	if err != nil || enabled || config.RPCURL != "" || config.CollectionAddress != "" {
		t.Fatalf("disabled config = %+v enabled=%v err=%v", config, enabled, err)
	}
}

func TestConfigRequiresCompleteHTTPSMainnetConfiguration(t *testing.T) {
	clearTronEnv(t)
	t.Setenv("TRON_USDT_RPC_URL", "https://api.trongrid.io")
	if _, enabled, err := ConfigFromEnv(); err == nil || enabled {
		t.Fatalf("partial configuration enabled=%v err=%v", enabled, err)
	}

	t.Setenv("TRON_USDT_COLLECTION_ADDRESS", testCollectionAddress)
	t.Setenv("TRON_USDT_RPC_URL", "http://api.trongrid.io")
	if _, enabled, err := ConfigFromEnv(); err == nil || enabled {
		t.Fatalf("plain HTTP configuration enabled=%v err=%v", enabled, err)
	}

	t.Setenv("TRON_USDT_RPC_URL", "https://api.trongrid.io")
	t.Setenv("TRON_API_KEY", "private-trongrid-key")
	config, enabled, err := ConfigFromEnv()
	if err != nil || !enabled || config.RPCURL != "https://api.trongrid.io" || config.CollectionAddress != testCollectionAddress {
		t.Fatalf("complete config = %+v enabled=%v err=%v", config, enabled, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", config), "private-trongrid-key") {
		t.Fatal("formatted configuration exposed TRON_API_KEY")
	}
}

func TestNewClientRevalidatesRPCOrigin(t *testing.T) {
	for _, rpcURL := range []string{"http://rpc.example", "https://user:pass@rpc.example", "https://rpc.example/path", "https://rpc.example?key=value"} {
		if _, err := NewClient(Config{RPCURL: rpcURL, CollectionAddress: testCollectionAddress}); err == nil {
			t.Fatalf("NewClient accepted unsafe RPC URL %q", rpcURL)
		}
	}
}

func TestAddressAcceptsConfiguredMainnetAndRejectsChecksumOrPrefix(t *testing.T) {
	if err := ValidateMainnetAddress(testCollectionAddress); err != nil {
		t.Fatalf("configured address rejected: %v", err)
	}
	if err := ValidateMainnetAddress(testCollectionAddress[:len(testCollectionAddress)-1] + "x"); err == nil {
		t.Fatal("bad checksum accepted")
	}
	payload := append([]byte{0xa0}, make([]byte, 20)...)
	if err := ValidateMainnetAddress(base58CheckForTest(payload)); err == nil {
		t.Fatal("non-mainnet prefix accepted")
	}
}

func base58CheckForTest(payload []byte) string {
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	raw := append(append([]byte{}, payload...), second[:4]...)
	n := new(big.Int).SetBytes(raw)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	var encoded []byte
	for n.Cmp(zero) > 0 {
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(n, base, remainder)
		encoded = append(encoded, "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"[remainder.Int64()])
		n = quotient
	}
	for _, value := range raw {
		if value != 0 {
			break
		}
		encoded = append(encoded, '1')
	}
	for left, right := 0, len(encoded)-1; left < right; left, right = left+1, right-1 {
		encoded[left], encoded[right] = encoded[right], encoded[left]
	}
	return string(encoded)
}

const (
	testTxHash    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	transferTopic = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
)

func tronHexForTest(t *testing.T, address string) string {
	t.Helper()
	payload, err := decodeBase58Check(address)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", payload[1:])
}

func transferLogForTest(t *testing.T, contract, recipient string, amount int64) string {
	t.Helper()
	return fmt.Sprintf(`{"address":%q,"topics":[%q,%q,%q],"data":%q}`,
		contract,
		transferTopic,
		strings.Repeat("0", 24)+strings.Repeat("1", 40),
		strings.Repeat("0", 24)+recipient,
		fmt.Sprintf("%064x", amount),
	)
}

func receiptForTest(t *testing.T, logs ...string) string {
	t.Helper()
	return fmt.Sprintf(`{"id":%q,"blockNumber":123,"blockTimeStamp":1760000000000,"receipt":{"result":"SUCCESS"},"log":[%s]}`, testTxHash, strings.Join(logs, ","))
}

func clientServerForTest(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	config := Config{RPCURL: server.URL, CollectionAddress: testCollectionAddress, apiKey: "test-api-key"}
	client, err := NewClient(config)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	testHTTP := server.Client()
	testHTTP.CheckRedirect = client.http.CheckRedirect
	testHTTP.Timeout = client.http.Timeout
	client.http = testHTTP
	return client, server
}

func TestClientAcceptsOneExactSolidifiedOfficialUSDTTransfer(t *testing.T) {
	contract := tronHexForTest(t, OfficialUSDTAddress)
	recipient := tronHexForTest(t, testCollectionAddress)
	client, server := clientServerForTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/walletsolidity/gettransactioninfobyid" || r.Method != http.MethodPost {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("TRON-PRO-API-KEY") != "test-api-key" {
			t.Fatal("missing API key header")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"value":"`+testTxHash+`"}` {
			t.Fatalf("body %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		response := strings.Replace(receiptForTest(t, transferLogForTest(t, contract, recipient, 2500000)), `{"id":`, `{"result":"SUCESS","id":`, 1)
		io.WriteString(w, response)
	})
	defer server.Close()

	receipt, err := client.Verify(context.Background(), testTxHash, 2500000)
	if err != nil || receipt.TxHash != testTxHash || receipt.AmountBaseUnits != 2500000 || receipt.ToAddress != testCollectionAddress || receipt.BlockNumber != 123 || receipt.LogIndex != 0 || !receipt.BlockTime.Equal(time.UnixMilli(1760000000000)) {
		t.Fatalf("receipt = %+v err=%v", receipt, err)
	}
}

func TestClientTreatsMissingSolidifiedReceiptAsPending(t *testing.T) {
	client, server := clientServerForTest(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{}`) })
	defer server.Close()
	if _, err := client.Verify(context.Background(), testTxHash, 1); !errors.Is(err, ErrPending) {
		t.Fatalf("empty receipt err=%v", err)
	}
}

func TestClientRejectsFailedWrongOrAmbiguousTransfers(t *testing.T) {
	contract := tronHexForTest(t, OfficialUSDTAddress)
	recipient := tronHexForTest(t, testCollectionAddress)
	other := strings.Repeat("2", 40)
	cases := map[string]string{
		"failed execution":  strings.Replace(receiptForTest(t, transferLogForTest(t, contract, recipient, 10)), `"SUCCESS"`, `"FAILED"`, 1),
		"failed top level":  strings.Replace(receiptForTest(t, transferLogForTest(t, contract, recipient, 10)), `{"id":`, `{"result":"FAILED","id":`, 1),
		"unknown top level": strings.Replace(receiptForTest(t, transferLogForTest(t, contract, recipient, 10)), `{"id":`, `{"result":"UNKNOWN","id":`, 1),
		"fake contract":     receiptForTest(t, transferLogForTest(t, strings.Repeat("3", 40), recipient, 10)),
		"wrong recipient":   receiptForTest(t, transferLogForTest(t, contract, other, 10)),
		"wrong amount":      receiptForTest(t, transferLogForTest(t, contract, recipient, 11)),
		"ambiguous":         receiptForTest(t, transferLogForTest(t, contract, recipient, 10), transferLogForTest(t, contract, recipient, 10)),
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			client, server := clientServerForTest(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, response) })
			defer server.Close()
			if _, err := client.Verify(context.Background(), testTxHash, 10); !errors.Is(err, ErrInvalidReceipt) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestClientRejectsMalformedOversizedRedirectAndTimeoutResponses(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"malformed": func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{`) },
		"oversized": func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"padding":"`+strings.Repeat("x", maxResponseBytes)+`"}`)
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"timeout": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			io.WriteString(w, `{}`)
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			client, server := clientServerForTest(t, handler)
			defer server.Close()
			if name == "timeout" {
				client.http.Timeout = 10 * time.Millisecond
			}
			if _, err := client.Verify(context.Background(), testTxHash, 1); !errors.Is(err, ErrProvider) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestClientRejectsMalformedHashAndAmountBeforeNetwork(t *testing.T) {
	client, server := clientServerForTest(t, func(http.ResponseWriter, *http.Request) { t.Fatal("network called") })
	defer server.Close()
	for _, test := range []struct {
		hash   string
		amount int64
	}{{"bad", 1}, {testTxHash, 0}, {testTxHash, -1}} {
		if _, err := client.Verify(context.Background(), test.hash, test.amount); !errors.Is(err, ErrInvalidReceipt) {
			t.Fatalf("input %+v err=%v", test, err)
		}
	}
}

func TestClientLatestSolidBlockUsesSolidityHeight(t *testing.T) {
	client, server := clientServerForTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/walletsolidity/getnowblock" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("TRON-PRO-API-KEY") != "test-api-key" {
			t.Fatal("missing API key header")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{}` {
			t.Fatalf("body %s", body)
		}
		io.WriteString(w, `{"block_header":{"raw_data":{"number":76543210}}}`)
	})
	defer server.Close()

	height, err := client.LatestSolidBlock(context.Background())
	if err != nil || height != 76543210 {
		t.Fatalf("height=%d err=%v", height, err)
	}
}

func TestClientLatestSolidBlockRejectsMalformedOrMissingHeight(t *testing.T) {
	for name, response := range map[string]string{
		"malformed": `{`,
		"missing":   `{"block_header":{"raw_data":{}}}`,
		"zero":      `{"block_header":{"raw_data":{"number":0}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, server := clientServerForTest(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, response) })
			defer server.Close()
			if _, err := client.LatestSolidBlock(context.Background()); !errors.Is(err, ErrProvider) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestClientScanSolidBlockReturnsOneUnambiguousOfficialUSDTTransfer(t *testing.T) {
	contract := tronHexForTest(t, OfficialUSDTAddress)
	recipient := tronHexForTest(t, testCollectionAddress)
	accepted := strings.Replace(receiptForTest(t, transferLogForTest(t, contract, recipient, 1234567)), `{"id":`, `{"result":"SUCESS","id":`, 1)
	response := `[` + receiptForTest(t, transferLogForTest(t, strings.Repeat("3", 40), recipient, 99)) + `,` + accepted + `]`
	client, server := clientServerForTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/walletsolidity/gettransactioninfobyblocknum" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("TRON-PRO-API-KEY") != "test-api-key" {
			t.Fatal("missing API key header")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"num":123}` {
			t.Fatalf("body %s", body)
		}
		io.WriteString(w, response)
	})
	defer server.Close()

	receipts, err := client.ScanSolidBlock(context.Background(), 123)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	got := receipts[0]
	if got.TxHash != testTxHash || got.BlockNumber != 123 || got.LogIndex != 0 || got.AmountBaseUnits != 1234567 || got.ToAddress != testCollectionAddress || got.ContractAddress != OfficialUSDTAddress {
		t.Fatalf("receipt=%+v", got)
	}
}

func TestClientScanSolidBlockIgnoresFailedUnrelatedAndAmbiguousTransactions(t *testing.T) {
	contract := tronHexForTest(t, OfficialUSDTAddress)
	recipient := tronHexForTest(t, testCollectionAddress)
	failed := strings.Replace(receiptForTest(t, transferLogForTest(t, contract, recipient, 10)), `"SUCCESS"`, `"FAILED"`, 1)
	topLevelFailed := strings.Replace(receiptForTest(t, transferLogForTest(t, contract, recipient, 11)), `{"id":`, `{"result":"FAILED","id":`, 1)
	ambiguous := receiptForTest(t, transferLogForTest(t, contract, recipient, 20), transferLogForTest(t, contract, recipient, 21))
	unrelated := receiptForTest(t, transferLogForTest(t, strings.Repeat("3", 40), recipient, 30))
	client, server := clientServerForTest(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `[`+failed+`,`+topLevelFailed+`,`+ambiguous+`,`+unrelated+`]`)
	})
	defer server.Close()
	receipts, err := client.ScanSolidBlock(context.Background(), 123)
	if err != nil || len(receipts) != 0 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
}

func TestClientScanSolidBlockRejectsWrongHeightMalformedAndOversizedResponses(t *testing.T) {
	wrongHeight := strings.Replace(receiptForTest(t), `"blockNumber":123`, `"blockNumber":122`, 1)
	for name, response := range map[string]string{
		"wrong height": `[` + wrongHeight + `]`,
		"malformed":    `[`,
		"oversized":    `[{"padding":"` + strings.Repeat("x", maxBlockResponseBytes) + `"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			client, server := clientServerForTest(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, response) })
			defer server.Close()
			if _, err := client.ScanSolidBlock(context.Background(), 123); !errors.Is(err, ErrProvider) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestClientScanSolidBlockRejectsInvalidHeightBeforeNetwork(t *testing.T) {
	client, server := clientServerForTest(t, func(http.ResponseWriter, *http.Request) { t.Fatal("network called") })
	defer server.Close()
	for _, height := range []int64{-1, 0} {
		if _, err := client.ScanSolidBlock(context.Background(), height); !errors.Is(err, ErrInvalidReceipt) {
			t.Fatalf("height=%d err=%v", height, err)
		}
	}
}
