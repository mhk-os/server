package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mhk-os/server/internal/content"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
)

// manifestFile is the -manifests file: the packs a bot announces in ContentHello, with their items so it can
// answer the server's requests for missing manifests. `mhk-bot dump-manifests` writes it.
type manifestFile struct {
	ContentVersion int32          `json:"contentVersion"`
	Packs          []manifestPack `json:"packs"`
}

type manifestPack struct {
	PackID       string         `json:"packId"`
	Commit       string         `json:"commit"`
	ManifestHash string         `json:"manifestHash"`
	Items        []manifestItem `json:"items"`
}

type manifestItem struct {
	Kind        uint8  `json:"kind"`
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

func (p *manifestPack) items() []proto.ManifestItem {
	items := make([]proto.ManifestItem, len(p.Items))
	for i, it := range p.Items {
		items[i] = proto.ManifestItem{Kind: it.Kind, ID: it.ID, Fingerprint: it.Fingerprint}
	}
	return items
}

// loadManifests reads and checks a manifest file: every hash must match its items, as the server checks.
func loadManifests(path string) (*manifestFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f manifestFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(f.Packs) == 0 {
		return nil, fmt.Errorf("%s: no packs", path)
	}
	for _, p := range f.Packs {
		h, err := content.ManifestHash(p.items())
		if err != nil {
			return nil, fmt.Errorf("%s: pack %s: %w", path, p.PackID, err)
		}
		if h != strings.ToLower(p.ManifestHash) {
			return nil, fmt.Errorf("%s: pack %s: manifestHash %s, items hash to %s", path, p.PackID, p.ManifestHash, h)
		}
	}
	return &f, nil
}

// refs is the ContentHello pack list.
func (f *manifestFile) refs() []proto.PackRef {
	refs := make([]proto.PackRef, len(f.Packs))
	for i, p := range f.Packs {
		refs[i] = proto.PackRef{PackID: p.PackID, Commit: p.Commit, ManifestHash: p.ManifestHash}
	}
	return refs
}

// manifests returns the PackManifests whose hashes the server asked for.
func (f *manifestFile) manifests(missing []string) []proto.PackManifest {
	var out []proto.PackManifest
	for _, p := range f.Packs {
		for _, h := range missing {
			if strings.EqualFold(h, p.ManifestHash) {
				out = append(out, proto.PackManifest{PackID: p.PackID, Commit: p.Commit, ContentVersion: f.ContentVersion,
					ManifestHash: p.ManifestHash, Items: p.items()})
				break
			}
		}
	}
	return out
}

// dumpManifests writes the most reported manifest of every pack for contentVersion from the server's database.
func dumpManifests(ctx context.Context, st *store.Store, contentVersion int32, packs []string, out string) error {
	f := manifestFile{ContentVersion: contentVersion}
	for _, id := range packs {
		m, err := st.MostReportedManifest(ctx, id, contentVersion)
		if err != nil {
			return fmt.Errorf("pack %s: %w", id, err)
		}
		p := manifestPack{PackID: m.PackID, Commit: m.Commit, ManifestHash: m.Hash}
		for _, it := range m.Items {
			p.Items = append(p.Items, manifestItem{Kind: it.Kind, ID: it.ID, Fingerprint: it.Fingerprint})
		}
		f.Packs = append(f.Packs, p)
	}
	b, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(b, '\n'), 0o644)
}
