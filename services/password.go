package services

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

const (
	passwordLen  = 8
	symbols      = "!@#$%&*"
	upperLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lowerLetters = "abcdefghijklmnopqrstuvwxyz"
	digits       = "0123456789"
)

// GenerateSecurePassword returns an 8-character password with at least one uppercase, one lowercase, one digit, one symbol.
// Uses crypto/rand. Do not log the returned string.
func GenerateSecurePassword() (string, error) {
	pick := func(s string) (byte, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(s))))
		if err != nil {
			return 0, err
		}
		return s[n.Int64()], nil
	}
	result := make([]byte, passwordLen)
	var err error
	result[0], err = pick(upperLetters)
	if err != nil {
		return "", err
	}
	result[1], err = pick(lowerLetters)
	if err != nil {
		return "", err
	}
	result[2], err = pick(digits)
	if err != nil {
		return "", err
	}
	result[3], err = pick(symbols)
	if err != nil {
		return "", err
	}
	all := upperLetters + lowerLetters + digits + symbols
	for i := 4; i < passwordLen; i++ {
		result[i], err = pick(all)
		if err != nil {
			return "", err
		}
	}
	// Shuffle Fisher-Yates with crypto/rand
	for i := passwordLen - 1; i >= 1; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", fmt.Errorf("shuffle: %w", err)
		}
		j := int(n.Int64())
		result[i], result[j] = result[j], result[i]
	}
	return string(result), nil
}
