package networkpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// Decode accepts one bounded JSON object and rejects unknown fields.
func Decode(data []byte, target interface{}) error {
	if len(data) > MaxBody || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return errors.New("expected a JSON object of at most 1 MiB")
	}

	// Policy.UnmarshalJSON already checks duplicate keys and UTF-8.
	if _, policy := target.(*Policy); !policy {
		if err := validateUniqueJSON(data); err != nil {
			return err
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(interface{})) != io.EOF {
		return errors.New("expected one JSON object")
	}

	return nil
}

func validateUniqueJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be valid UTF-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("expected one JSON value")
		}

		return err
	}

	return nil
}

func consumeJSON(decoder *json.Decoder, depth int) error {
	if depth > 100 {
		return errors.New("JSON nesting exceeds 100 levels")
	}

	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}

			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key must be a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON field %q", key)
			}

			seen[key] = true
			if err := consumeJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}

	end, err := decoder.Token()
	if err != nil {
		return err
	}
	if end != json.Delim(map[json.Delim]rune{'{': '}', '[': ']'}[delimiter]) {
		return errors.New("mismatched JSON delimiter")
	}

	return nil
}

func policyPath(root, id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return "", errors.New("invalid server UUID")
	}

	return filepath.Join(root, "network-policy", id+".json"), nil
}

// Load reads a server's policy, returning Empty when no saved policy exists.
func Load(root, id string) (Policy, error) {
	policy := Empty()

	path, err := policyPath(root, id)
	if err != nil {
		return policy, err
	}

	if err := privatePolicyDirectory(filepath.Dir(path)); err != nil {
		if os.IsNotExist(err) {
			return policy, nil
		}

		return policy, err
	}

	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return policy, nil
	}
	if err != nil {
		return policy, err
	}

	defer func() {
		_ = file.Close()
	}()

	info, err := file.Stat()
	if err != nil {
		return policy, err
	}
	if !info.Mode().IsRegular() {
		return policy, errors.New("policy state must be a regular file")
	}

	data, err := io.ReadAll(io.LimitReader(file, MaxBody+1))
	if err != nil {
		return policy, err
	}

	err = Decode(data, &policy)
	return policy, err
}

// Save atomically persists a desired policy before the caller touches the kernel.
// The caller must hold its server lock to serialize revisions and reconciliation.
func Save(root, id string, policy Policy) error {
	if err := policy.validateStatic(); err != nil {
		return err
	}

	path, err := policyPath(root, id)
	if err != nil {
		return err
	}

	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := privatePolicyDirectory(filepath.Dir(path)); err != nil {
		return err
	}

	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if len(data) > MaxBody {
		return errors.New("policy exceeds 1 MiB")
	}
	return writeState(path, data)
}

func writeState(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".policy-")
	if err != nil {
		return err
	}

	defer func() {
		_ = os.Remove(file.Name())
	}()

	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}

	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}

	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}

	defer func() {
		_ = dir.Close()
	}()

	return dir.Sync()
}

func privatePolicyDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() ||
		info.Mode().Perm()&0077 != 0 ||
		!ok ||
		owner.Uid != uint32(os.Geteuid()) {
		return errors.New("policy directory must be private, owned by Wings, and not a symlink")
	}

	return nil
}

// Remove deletes persisted state after the server and its owned resources are gone.
func Remove(root, id string) error {
	path, err := policyPath(root, id)
	if err != nil {
		return err
	}

	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}

	return err
}
