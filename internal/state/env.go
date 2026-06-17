//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/wnstify/wdm/pkg/types"
)

const stackEnvFilename = ".env"

// ReadStackEnv reads and parses <stackPath>/.env as KEY=VALUE lines.
// Parsing rules:
//   - split each non-comment line on the first "=" only
//   - trim whitespace around the key only
//   - preserve the value bytes exactly after the first "="
//   - ignore blank lines and full-line comments (after optional leading whitespace)
//
// This helper is read-only and never modifies disk.
func ReadStackEnv(stackPath string) (map[string]string, error) {
	if stackPath == "" || !filepath.IsAbs(stackPath) {
		return nil, fmt.Errorf("state.ReadStackEnv: stackPath must be a non-empty absolute path, got %q", stackPath)
	}

	envPath := filepath.Join(stackPath, stackEnvFilename)
	info, err := os.Lstat(envPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, wrapStackEnvValidationError(
				"existing stack env file is missing",
				fmt.Sprintf("create or restore %q before retrying", envPath),
				fmt.Errorf("state.ReadStackEnv: stating %q: %w", envPath, err),
			)
		}
		return nil, wrapStackEnvValidationError(
			"unable to inspect existing stack env file",
			fmt.Sprintf("ensure %q exists and is readable", envPath),
			fmt.Errorf("state.ReadStackEnv: stating %q: %w", envPath, err),
		)
	}
	if !info.Mode().IsRegular() {
		return nil, wrapStackEnvValidationError(
			"existing stack env file must be a regular file",
			fmt.Sprintf("replace %q with a regular file containing KEY=VALUE entries", envPath),
			fmt.Errorf("state.ReadStackEnv: %q is not a regular file", envPath),
		)
	}

	// G304 is suppressed: envPath is composed from an absolute stack path.
	raw, err := os.ReadFile(envPath) //nolint:gosec // G304: absolute stack path is validated by caller contract
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, wrapStackEnvValidationError(
				"existing stack env file is missing",
				fmt.Sprintf("create or restore %q before retrying", envPath),
				fmt.Errorf("state.ReadStackEnv: reading %q: %w", envPath, err),
			)
		}
		return nil, wrapStackEnvValidationError(
			"unable to read existing stack env file",
			fmt.Sprintf("ensure %q is readable", envPath),
			fmt.Errorf("state.ReadStackEnv: reading %q: %w", envPath, err),
		)
	}

	lines := strings.Split(string(raw), "\n")
	parsed := make(map[string]string, len(lines))
	for i, line := range lines {
		lineNumber := i + 1

		if strings.TrimSpace(line) == "" {
			continue
		}
		if isStackEnvCommentLine(line) {
			continue
		}

		separator := strings.IndexByte(line, '=')
		if separator < 0 {
			return nil, wrapStackEnvValidationError(
				"existing stack env file is malformed",
				fmt.Sprintf("line %d must use KEY=VALUE format", lineNumber),
				nil,
			)
		}

		key := strings.TrimSpace(line[:separator])
		if key == "" {
			return nil, wrapStackEnvValidationError(
				"existing stack env file is malformed",
				fmt.Sprintf("line %d has an empty key", lineNumber),
				nil,
			)
		}
		if _, exists := parsed[key]; exists {
			return nil, wrapStackEnvValidationError(
				"existing stack env file is malformed",
				fmt.Sprintf("line %d duplicates key %q", lineNumber, key),
				nil,
			)
		}

		parsed[key] = line[separator+1:]
	}

	return parsed, nil
}

func isStackEnvCommentLine(line string) bool {
	trimmedLeading := strings.TrimLeftFunc(line, unicode.IsSpace)
	return strings.HasPrefix(trimmedLeading, "#")
}

func wrapStackEnvValidationError(message string, hint string, cause error) error {
	if cause == nil {
		return types.NewError(types.ErrCodeUsageValidation, message, hint)
	}

	return types.WrapError(types.ErrCodeUsageValidation, message, hint, cause)
}
