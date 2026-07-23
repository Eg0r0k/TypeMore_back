package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. These follow the OWASP Password Storage Cheat Sheet
// (2024) recommendation for argon2id: 19 MiB memory, 2 iterations, 1 degree of
// parallelism. They are *encoded into every hash* (PHC string format), so they
// can be raised later and old hashes still verify against their own parameters.
const (
	argonMemoryKiB  = 19 * 1024 // 19 MiB, expressed in KiB as argon2 wants
	argonIterations = 2
	argonParallel   = 1
	argonSaltLen    = 16
	argonKeyLen     = 32
)

// errInvalidHash is returned when a stored hash string is not in the expected
// PHC format (data corruption or a hash from an incompatible scheme).
var errInvalidHash = errors.New("auth: invalid password hash format")

// HashPassword derives an argon2id hash of password and returns it in PHC string
// form: $argon2id$v=19$m=...,t=...,p=...$salt$hash (all base64, no padding). A
// fresh random salt is generated per call.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonIterations, argonMemoryKiB, argonParallel, argonKeyLen)

	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonIterations, argonParallel,
		b64.EncodeToString(salt), b64.EncodeToString(hash),
	), nil
}

// VerifyPassword reports whether password matches the PHC-encoded argon2id hash.
// It reads the cost parameters from the encoded hash (not the current consts) so
// hashes made with older parameters still verify. The final comparison is
// constant-time. A malformed encoded hash returns an error.
func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.iterations, params.memoryKiB, params.parallel, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memoryKiB  uint32
	iterations uint32
	parallel   uint8
}

// decodeHash parses a PHC argon2id string back into its parameters, salt, and
// derived key.
func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, errInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, errInvalidHash
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.iterations, &p.parallel); err != nil {
		return argonParams{}, nil, nil, errInvalidHash
	}

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, errInvalidHash
	}
	hash, err := b64.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, errInvalidHash
	}
	return p, salt, hash, nil
}
