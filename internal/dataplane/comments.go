package dataplane

import (
	"context"
	"errors"
	"strings"
)

type nftTextComments struct {
	Owner string
	Relay string
}

func nftLine(line string) (string, int, error) {
	quoted, escaped, commentAt, delta := false, false, -1, 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quoted {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '#':
			commentAt = i
			i = len(line)
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	if quoted || escaped {
		return "", 0, errors.New("unterminated nft quoted text")
	}
	code := line
	if commentAt >= 0 {
		code = line[:commentAt]
		fields := strings.Fields(line[commentAt+1:])
		if len(fields) != 2 || fields[0] != "handle" || fields[1] == "" {
			return "", 0, errors.New("unsupported nft text annotation")
		}
		for _, r := range fields[1] {
			if r < '0' || r > '9' {
				return "", 0, errors.New("invalid nft handle annotation")
			}
		}
	}
	return strings.TrimSpace(code), delta, nil
}

func commentValue(line string) (string, bool) {
	const prefix = "comment \""
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "\"") {
		return "", false
	}
	v := strings.TrimSuffix(strings.TrimPrefix(line, prefix), "\"")
	if v == "" || strings.ContainsAny(v, "\\\"") {
		return "", false
	}
	return v, true
}

func parseNFTTextComments(raw []byte) (nftTextComments, error) {
	var result nftTextComments
	depth, ownerCount, relayCount := 0, 0, 0
	started, closed, inRelay := false, false, false
	for _, original := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(original)
		if line == "" {
			continue
		}
		if closed || strings.ContainsRune(line, 0) {
			return result, errors.New("invalid nft text inventory")
		}
		var delta int
		var err error
		line, delta, err = nftLine(line)
		if err != nil {
			return result, err
		}
		if !started {
			if line != "table ip "+Table+" {" {
				return result, errors.New("unexpected nft table text")
			}
			started, depth = true, 1
			continue
		}
		if depth == 1 {
			if value, ok := commentValue(line); ok {
				ownerCount++
				result.Owner = value
			}
			if line == "chain relay {" {
				inRelay = true
			}
		} else if depth == 2 && inRelay {
			if value, ok := commentValue(line); ok {
				relayCount++
				result.Relay = value
			}
		}
		depth += delta
		if depth < 0 {
			return result, errors.New("invalid nft text nesting")
		}
		if inRelay && depth < 2 {
			inRelay = false
		}
		if depth == 0 {
			if line != "}" {
				return result, errors.New("invalid nft table close")
			}
			closed = true
		}
	}
	if !started || !closed || depth != 0 || ownerCount != 1 || relayCount > 1 {
		return result, errors.New("incomplete nft text inventory")
	}
	return result, nil
}

func readNFTTextComments(ctx context.Context, runner Runner) (nftTextComments, error) {
	raw, err := runner.Run(ctx, []string{"-a", "list", "table", "ip", Table}, "")
	if err != nil {
		return nftTextComments{}, err
	}
	return parseNFTTextComments(raw)
}
