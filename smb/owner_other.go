//go:build !windows

package smb

// processSIDs is empty: Unix stats carry the IDs the descriptor reports.
func processSIDs() (owner, group []byte, err error) { return nil, nil, nil }
