package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLength is enforced at hash time. No complexity rules.
const MinPasswordLength = 8

// BcryptCost is the bcrypt work factor. 12 = ~250ms on modern hardware,
// the current OWASP recommendation.
const BcryptCost = 12

// ErrPasswordTooShort is returned by HashPassword for sub-minimum inputs.
var ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)

// ErrPasswordMismatch is returned by CheckPassword on a failed compare.
// Distinct from any other bcrypt error so callers can map it to 401.
var ErrPasswordMismatch = errors.New("auth: password mismatch")

// HashPassword applies bcrypt at BcryptCost after enforcing the length rule.
func HashPassword(plain string) (string, error) {
	if len(plain) < MinPasswordLength {
		return "", ErrPasswordTooShort
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword returns nil if plain matches the stored hash,
// ErrPasswordMismatch if it doesn't, or a wrapped error on any other
// bcrypt failure.
func CheckPassword(hash, plain string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrPasswordMismatch
	}
	if err != nil {
		return fmt.Errorf("auth: compare password: %w", err)
	}
	return nil
}
