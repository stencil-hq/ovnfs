//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// nfsMountReport is the current, not original, configuration from nfsstat -f JSON -m.
type nfsMountReport struct {
	Mount   string `json:"Mount Point"`
	Current struct {
		General struct {
			Flags []string `json:"Flags"`
		} `json:"General mount flags"`
		Parameters []string `json:"NFS parameters"`
	} `json:"Current mount parameters"`
}

// nfsMountMismatch identifies the first expected option absent from the live
// mount. Unreadable or missing reports fail closed, without unmounting anything.
func nfsMountMismatch(data []byte, mount string, command []string) (string, error) {
	var reports map[string]nfsMountReport
	if err := json.Unmarshal(data, &reports); err != nil {
		return "", fmt.Errorf("decoding NFS mount parameters: %w", err)
	}
	var actual map[string]string
	for _, report := range reports {
		if report.Mount != mount {
			continue
		}
		if len(report.Current.Parameters) == 0 {
			return "", fmt.Errorf("NFS mount %s has no current parameters", mount)
		}
		actual = make(map[string]string)
		for _, options := range [][]string{report.Current.General.Flags, report.Current.Parameters} {
			for _, option := range options {
				key, value, _ := strings.Cut(option, "=")
				actual[key] = value
			}
		}
		break
	}
	if actual == nil {
		return "", fmt.Errorf("NFS mount %s missing from nfsstat", mount)
	}
	var expected string
	for i, arg := range command {
		if arg == "-o" && i+1 < len(command) {
			expected = command[i+1]
			break
		}
	}
	if expected == "" {
		return "", fmt.Errorf("NFS mount command has no options")
	}
	for option := range strings.SplitSeq(expected, ",") {
		key, want, _ := strings.Cut(option, "=")
		if key == "atime" {
			if _, disabled := actual["noatime"]; disabled {
				return "access times are disabled", nil
			}
			continue
		}
		keys := []string{key}
		if key == "rwsize" {
			keys = []string{"rsize", "wsize"}
		}
		for _, key := range keys {
			got, present := actual[key]
			if !present {
				return fmt.Sprintf("missing %s", key), nil
			}
			if got != want {
				return fmt.Sprintf("%s=%s, expected %s", key, got, want), nil
			}
		}
	}
	return "", nil
}
