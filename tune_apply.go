package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/manifest"
)

var tuneKeys = []string{"proto", "memory", "nfs_io_size"}

func tuneSettings(home string) (map[string][]string, error) {
	doc := manifest.UserDoc(home)
	if err := doc.Read(); err != nil {
		return nil, err
	}
	return tuneDocSettings(doc)
}

func tuneDocSettings(doc *manifest.Doc) (map[string][]string, error) {
	out := make(map[string][]string, len(tuneKeys))
	for _, key := range tuneKeys {
		values, err := manifest.Resolve(key, doc)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			out[key] = append(out[key], value.Value)
		}
	}
	return out, nil
}

func unchangedTuneSettings(doc *manifest.Doc, before map[string][]string) error {
	current, err := tuneDocSettings(doc)
	if err != nil {
		return err
	}
	for _, key := range tuneKeys {
		if !slices.Equal(current[key], before[key]) {
			return fmt.Errorf("%s changed while tuning; configuration was not overwritten", key)
		}
	}
	return nil
}

// saveTune changes only tuned keys and saves them in one existing atomic Doc
// transaction. Re-reading preserves comments and unrelated concurrent edits.
func saveTune(home string, before map[string][]string, chosen tuneConfig) error {
	doc := manifest.UserDoc(home)
	if err := doc.Read(); err != nil {
		return err
	}
	if err := unchangedTuneSettings(doc, before); err != nil {
		return err
	}
	for i, value := range []string{chosen.Proto, chosen.Memory.String(), chosen.NFSIOSize.String()} {
		if err := doc.Set(tuneKeys[i], value); err != nil {
			return err
		}
	}
	return doc.Save()
}

// applyTune activates explicit serve overrides first. A failed stop/start never
// replaces the user's settings with a configuration that did not come up.
func applyTune(c *cli, home string, before map[string][]string, chosen tuneConfig) error {
	doc := manifest.UserDoc(home)
	if err := doc.Read(); err != nil {
		return err
	}
	if err := unchangedTuneSettings(doc, before); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, serveLoadWait+daemonWait)
	defer cancel()
	client := &cli{ctx: ctx, state: c.state, mount: c.mount}
	defer client.close()
	lock, err := lockDaemon(c.state)
	if err != nil {
		return err
	}
	defer lock.Close()
	if accepting(c.state) {
		connection, err := client.client()
		if err != nil {
			return err
		}
		keep := slices.Contains(connection.Welcome().Features, "stop.keep_mount")
		if _, err := stopDaemon(client, api.StopArgs{KeepMount: keep}); err != nil {
			return err
		}
	}
	overrides := []string{"--proto", chosen.Proto, "--memory", chosen.Memory.String(), "--nfs-io-size", chosen.NFSIOSize.String()}
	connection, _, err := spawnDaemon(ctx, c.state, c.mount, overrides)
	if err != nil {
		return fmt.Errorf("tune: activation failed; configuration unchanged: %w", err)
	}
	client.conn = connection
	if err := saveTune(home, before, chosen); err != nil {
		return fmt.Errorf("daemon is running the measured settings, but persistence failed: %w", err)
	}
	return nil
}
