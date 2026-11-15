// Package tronpayments verifies receive-only TRON mainnet USDT payments.
package tronpayments

import (
	"errors"
	"fmt"
	"net/url"
	"os"
)

const OfficialUSDTAddress = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

type Config struct {
	RPCURL            string
	CollectionAddress string
	apiKey            string
}

func validRPCOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func (c Config) String() string {
	return fmt.Sprintf("{RPCURL:%s CollectionAddress:%s APIKeyConfigured:%t}", c.RPCURL, c.CollectionAddress, c.apiKey != "")
}

// ConfigFromEnv treats the connector as disabled only when all values are
// absent. Partial or unsafe configuration fails closed.
func ConfigFromEnv() (Config, bool, error) {
	c := Config{
		RPCURL:            os.Getenv("TRON_USDT_RPC_URL"),
		CollectionAddress: os.Getenv("TRON_USDT_COLLECTION_ADDRESS"),
		apiKey:            os.Getenv("TRON_API_KEY"),
	}
	if c.RPCURL == "" && c.CollectionAddress == "" && c.apiKey == "" {
		return Config{}, false, nil
	}
	if c.RPCURL == "" || c.CollectionAddress == "" {
		return Config{}, false, errors.New("TRON_USDT_RPC_URL and TRON_USDT_COLLECTION_ADDRESS are required together")
	}
	if !validRPCOrigin(c.RPCURL) {
		return Config{}, false, errors.New("TRON_USDT_RPC_URL must be an exact HTTPS origin")
	}
	if err := ValidateMainnetAddress(c.CollectionAddress); err != nil {
		return Config{}, false, errors.New("TRON_USDT_COLLECTION_ADDRESS is not a valid TRON mainnet address")
	}
	return c, true, nil
}
