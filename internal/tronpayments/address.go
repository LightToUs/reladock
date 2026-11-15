package tronpayments

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"math/big"
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func ValidateMainnetAddress(address string) error {
	payload, err := decodeBase58Check(address)
	if err != nil || len(address) != 34 || len(payload) != 21 || payload[0] != 0x41 {
		return errors.New("invalid TRON mainnet address")
	}
	return nil
}

func decodeBase58Check(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("empty Base58Check value")
	}
	n := big.NewInt(0)
	base := big.NewInt(58)
	for _, char := range []byte(value) {
		index := -1
		for i := 0; i < len(base58Alphabet); i++ {
			if base58Alphabet[i] == char {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, errors.New("invalid Base58 character")
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(index)))
	}
	raw := n.Bytes()
	leading := 0
	for leading < len(value) && value[leading] == '1' {
		leading++
	}
	if leading > 0 {
		raw = append(make([]byte, leading), raw...)
	}
	if len(raw) < 5 {
		return nil, errors.New("short Base58Check value")
	}
	payload := raw[:len(raw)-4]
	want := raw[len(raw)-4:]
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	if subtle.ConstantTimeCompare(want, second[:4]) != 1 {
		return nil, errors.New("invalid Base58Check checksum")
	}
	return payload, nil
}

func encodeBase58Check(payload []byte) string {
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	raw := append(append([]byte{}, payload...), second[:4]...)
	n := new(big.Int).SetBytes(raw)
	base := big.NewInt(58)
	var encoded []byte
	for n.Sign() > 0 {
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(n, base, remainder)
		encoded = append(encoded, base58Alphabet[remainder.Int64()])
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
