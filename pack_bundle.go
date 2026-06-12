package cherry

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

const (
	// BundleFormatVersion identifies the zstd bundle envelope format. It is
	// separate from the binary pack format version stored in Manifest.
	BundleFormatVersion = "pack-v1"
	bundleMagic         = "OPF1"
)

// BundleMetadata is the small delivery envelope around a compact pack blob. It
// tells an enforcement point what scope selection the bundle represents and
// carries the manifest used to validate the embedded pack.
type BundleMetadata struct {
	FormatVersion string   `json:"format_version"`
	ScopeKind     string   `json:"scope_kind"`
	ScopeID       string   `json:"scope_id"`
	Scopes        []string `json:"scopes"`
	PackManifest  Manifest `json:"pack_manifest"`
}

// Bundle combines delivery metadata with an uncompressed pack blob. Use
// EncodeBundleZstd to serialize it for transport or storage.
type Bundle struct {
	Metadata BundleMetadata
	Blob     []byte
}

// OpenedBundle is the result of opening a delivered bundle. Reader is validated
// and ready for enforcement queries; Blob is retained because Reader references
// that immutable byte slice.
type OpenedBundle struct {
	Metadata BundleMetadata
	Blob     []byte
	Reader   Reader
}

// NewBundle constructs a bundle envelope for an already-built pack blob.
// scopeKind and scopeID describe the requested control-plane selection, while
// scopes lists the concrete enforcement scopes contained in the blob.
func NewBundle(scopeKind string, scopeID string, scopes []string, blob []byte, manifest Manifest) Bundle {
	return Bundle{
		Metadata: BundleMetadata{
			FormatVersion: BundleFormatVersion,
			ScopeKind:     scopeKind,
			ScopeID:       scopeID,
			Scopes:        append([]string{}, scopes...),
			PackManifest:  manifest,
		},
		Blob: blob,
	}
}

// EncodeBundleZstd serializes bundle metadata plus blob and compresses the
// result with zstd. The returned bytes are the delivery artifact consumed by
// OpenBundleZstd.
func EncodeBundleZstd(bundle Bundle) ([]byte, error) {
	payload, err := EncodeBundle(bundle)
	if err != nil {
		return nil, err
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	return encoder.EncodeAll(payload, nil), nil
}

// DecodeBundleZstd decompresses a zstd bundle artifact and decodes its metadata
// and embedded pack blob. It does not open or validate the embedded pack; use
// OpenBundleZstd for the enforcement-point load path.
func DecodeBundleZstd(compressed []byte) (Bundle, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return Bundle{}, fmt.Errorf("create zstd decoder: %w", err)
	}
	defer decoder.Close()
	payload, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return Bundle{}, fmt.Errorf("decode zstd: %w", err)
	}
	return DecodeBundle(payload)
}

// OpenBundleZstd decodes a zstd bundle artifact, validates the embedded pack
// against the bundle manifest, and returns a ready-to-query Reader.
func OpenBundleZstd(compressed []byte) (OpenedBundle, error) {
	bundle, err := DecodeBundleZstd(compressed)
	if err != nil {
		return OpenedBundle{}, err
	}
	reader, err := OpenWithManifest(bundle.Blob, bundle.Metadata.PackManifest)
	if err != nil {
		return OpenedBundle{}, fmt.Errorf("open pack: %w", err)
	}
	return OpenedBundle{
		Metadata: bundle.Metadata,
		Blob:     bundle.Blob,
		Reader:   reader,
	}, nil
}

// EncodeBundle serializes bundle metadata and the raw pack blob without
// compression. The payload is intended as the inner format used by
// EncodeBundleZstd.
func EncodeBundle(bundle Bundle) ([]byte, error) {
	if bundle.Metadata.FormatVersion == "" {
		bundle.Metadata.FormatVersion = BundleFormatVersion
	}
	meta, err := json.Marshal(bundle.Metadata)
	if err != nil {
		return nil, fmt.Errorf("encode bundle metadata: %w", err)
	}
	if len(meta) > int(^uint32(0)) {
		return nil, fmt.Errorf("bundle metadata too large")
	}

	var payload bytes.Buffer
	payload.WriteString(bundleMagic)
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(meta)))
	payload.Write(lenBuf[:])
	payload.Write(meta)
	payload.Write(bundle.Blob)
	return payload.Bytes(), nil
}

// DecodeBundle parses an uncompressed bundle payload produced by EncodeBundle.
// It validates the bundle envelope version but does not open the embedded pack.
func DecodeBundle(payload []byte) (Bundle, error) {
	if len(payload) < len(bundleMagic)+4 || string(payload[:len(bundleMagic)]) != bundleMagic {
		return Bundle{}, fmt.Errorf("not a pack bundle")
	}
	metaLen := binary.LittleEndian.Uint32(payload[len(bundleMagic) : len(bundleMagic)+4])
	metaStart := len(bundleMagic) + 4
	metaEnd := metaStart + int(metaLen)
	if metaEnd > len(payload) {
		return Bundle{}, fmt.Errorf("truncated pack bundle metadata")
	}

	var metadata BundleMetadata
	if err := json.Unmarshal(payload[metaStart:metaEnd], &metadata); err != nil {
		return Bundle{}, fmt.Errorf("decode bundle metadata: %w", err)
	}
	if metadata.FormatVersion != BundleFormatVersion {
		return Bundle{}, fmt.Errorf("unsupported pack bundle format %q", metadata.FormatVersion)
	}
	return Bundle{
		Metadata: metadata,
		Blob:     payload[metaEnd:],
	}, nil
}
