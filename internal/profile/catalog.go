package profile

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// Candidate is one reconciled profile entry as published in
// VcrGpuCluster status: the configured image, its resolved digest, the
// parsed metadata, and a reconciliation message when the entry is unusable.
type Candidate struct {
	Image    string
	Digest   string
	Metadata *Metadata
	Message  string
}

// Selection is the one profile admission pins for a workload.
type Selection struct {
	ID       string
	Image    string
	Digest   string
	Metadata *Metadata
}

// Catalog is the reconciled set of profile candidates. Construction never
// discards a candidate silently: unusable entries stay in the catalog as
// rejected candidates so selection errors can explain what was considered.
//
// A catalog is immutable after construction and safe for concurrent selection.
type Catalog struct {
	entries []catalogEntry
}

type catalogEntry struct {
	candidate Candidate
	// rejected is non-empty when the candidate cannot be selected. It holds the
	// reason, which is reported verbatim in selection failures.
	rejected string
}

// candidateMiss records why a candidate did not match, and how many criteria it
// did satisfy so that diagnostics can be ordered by closeness.
type candidateMiss struct {
	label   string
	image   string
	digest  string
	score   int
	reasons []string
}

// NewCatalog builds the selection catalog from reconciled candidates.
//
// An entry is rejected (but retained for diagnostics) when reconciliation
// published a message, when metadata is missing or violates the profile image
// contract, or when the image is not digest-pinned. Rejected entries never
// participate in selection.
//
// Duplicate digests are allowed: the same recorded profile may be referenced by
// more than one tag. Entries that agree on both id and digest are collapsed to
// the first occurrence so that an identical profile cannot become spuriously
// ambiguous. The same id resolved to different digests is a catalog error: the
// recorded identity would be ambiguous. So is one id and digest published with
// two different metadata documents.
func NewCatalog(candidates []Candidate) (*Catalog, error) {
	catalog := &Catalog{}
	digestByID := make(map[string]string, len(candidates))
	imageByID := make(map[string]string, len(candidates))
	seen := make(map[string]*Metadata, len(candidates))

	for _, candidate := range candidates {
		entry := catalogEntry{candidate: candidate}

		switch {
		case candidate.Message != "":
			entry.rejected = candidate.Message
		case candidate.Metadata == nil:
			entry.rejected = "no profile metadata was published for this image"
		default:
			if err := candidate.Metadata.validate(); err != nil {
				entry.rejected = err.Error()
				break
			}
			if strings.TrimSpace(candidate.Digest) == "" {
				entry.rejected = "the profile image is not pinned to a digest: admission always pins a digest"
				break
			}

			id := candidate.Metadata.ID
			if prior, ok := digestByID[id]; ok && prior != candidate.Digest {
				return nil, fmt.Errorf("profile catalog: id %q resolves to two digests (%s from image %q, %s from image %q): one recorded identity must have one digest",
					id, prior, imageByID[id], candidate.Digest, candidate.Image)
			}
			digestByID[id] = candidate.Digest
			imageByID[id] = candidate.Image

			key := id + "\x00" + candidate.Digest
			if prior, duplicate := seen[key]; duplicate {
				if !prior.equal(candidate.Metadata) {
					return nil, fmt.Errorf("profile catalog: id %q with digest %s is published with conflicting metadata: the same recorded profile must have one document",
						id, candidate.Digest)
				}
				// Same recorded profile reached through another tag.
				continue
			}
			seen[key] = candidate.Metadata
		}

		catalog.entries = append(catalog.entries, entry)
	}

	return catalog, nil
}

// IDs returns the profile identities the catalog offers, sorted.
//
// It exists for denial messages: when a namespace asks for an identity the catalog does
// not carry, the useful answer is the list of identities it does carry.
func (c *Catalog) IDs() []string {
	ids := make([]string, 0, len(c.entries))
	for _, entry := range c.entries {
		if entry.candidate.Metadata == nil {
			continue
		}
		ids = append(ids, entry.candidate.Metadata.ID)
	}
	sort.Strings(ids)
	return ids
}

// Candidates returns the catalog entries in the order they were supplied,
// including rejected candidates.
func (c *Catalog) Candidates() []Candidate {
	out := make([]Candidate, len(c.entries))
	for i, entry := range c.entries {
		out[i] = entry.candidate
	}
	return out
}

// Select returns the single profile that can serve the workload requirements.
//
// Selection is all-or-nothing: zero compatible profiles and more than one
// compatible profile are both rejections. Picking an arbitrary match would make
// recorded timing and content depend on catalog ordering.
//
// Field semantics follow Requirements: an empty optional string or a nil pointer
// leaves the profile's recorded value standing. Quantization is the exception —
// an empty value means unquantized and is always compared, so an unquantized
// workload is never served by a quantized recording. GPU type and device count
// are never optional: without them no recorded topology can be identified.
//
// vLLM compatibility is an exact release match. A profile recorded against a
// different frontend release cannot be proven compatible, so it is rejected
// rather than approximated.
func (c *Catalog) Select(req Requirements) (Selection, error) {
	if err := req.validate(); err != nil {
		return Selection{}, err
	}

	var matches []Selection
	var misses []candidateMiss

	for _, entry := range c.entries {
		label := candidateLabel(entry.candidate)
		if entry.rejected != "" {
			misses = append(misses, candidateMiss{
				label:   label,
				image:   entry.candidate.Image,
				digest:  entry.candidate.Digest,
				reasons: []string{entry.rejected},
			})
			continue
		}

		reasons, score := matchRequirements(entry.candidate.Metadata, req)
		if len(reasons) == 0 {
			matches = append(matches, Selection{
				ID:       entry.candidate.Metadata.ID,
				Image:    entry.candidate.Image,
				Digest:   entry.candidate.Digest,
				Metadata: entry.candidate.Metadata,
			})
			continue
		}
		misses = append(misses, candidateMiss{
			label:   label,
			image:   entry.candidate.Image,
			digest:  entry.candidate.Digest,
			score:   score,
			reasons: reasons,
		})
	}

	switch len(matches) {
	case 1:
		return matches[0], nil

	case 0:
		if len(c.entries) == 0 {
			return Selection{}, fmt.Errorf("no reconciled profile catalog: no profile candidates are available for model %q on %s x%d; profile metadata must be reconciled before admission",
				req.Model, req.GPUType, req.GPUCount)
		}
		return Selection{}, fmt.Errorf("no compatible profile for %s: %s", req.describe(), describeMisses(misses))

	default:
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].ID != matches[j].ID {
				return matches[i].ID < matches[j].ID
			}
			if matches[i].Digest != matches[j].Digest {
				return matches[i].Digest < matches[j].Digest
			}
			return matches[i].Image < matches[j].Image
		})
		described := make([]string, len(matches))
		for i, match := range matches {
			described[i] = fmt.Sprintf("%q (image %q)", match.ID, match.Image)
		}
		return Selection{}, fmt.Errorf("ambiguous profile selection for %s: %d compatible profiles match: %s; exactly one compatible profile is required",
			req.describe(), len(matches), strings.Join(described, ", "))
	}
}

// maxReportedCandidates bounds the diagnostics attached to a selection failure.
const maxReportedCandidates = 5

func describeMisses(misses []candidateMiss) string {
	// sort.SliceStable keeps the comparison total and ordering deterministic.
	sort.SliceStable(misses, func(i, j int) bool {
		if misses[i].score != misses[j].score {
			return misses[i].score > misses[j].score
		}
		if misses[i].label != misses[j].label {
			return misses[i].label < misses[j].label
		}
		if misses[i].image != misses[j].image {
			return misses[i].image < misses[j].image
		}
		return misses[i].digest < misses[j].digest
	})

	reported := misses
	if len(reported) > maxReportedCandidates {
		reported = reported[:maxReportedCandidates]
	}
	lines := make([]string, len(reported))
	for i, miss := range reported {
		lines[i] = fmt.Sprintf("%s (image %q): %s", miss.label, miss.image, strings.Join(miss.reasons, "; "))
	}
	summary := fmt.Sprintf("%d candidate(s) considered, closest first: %s", len(misses), strings.Join(lines, " | "))
	if len(misses) > len(reported) {
		summary += fmt.Sprintf(" (showing %d of %d)", len(reported), len(misses))
	}
	return summary
}

func candidateLabel(candidate Candidate) string {
	if candidate.Metadata != nil && candidate.Metadata.ID != "" {
		return fmt.Sprintf("profile %q", candidate.Metadata.ID)
	}
	return fmt.Sprintf("image %q", candidate.Image)
}

// validate rejects requirements that cannot describe a serveable workload. These
// are workload errors, not profile mismatches, so they are reported before the
// catalog is examined.
// equal reports whether two metadata documents describe the same profile.
func (m *Metadata) equal(other *Metadata) bool {
	return reflect.DeepEqual(m, other)
}

func (r Requirements) validate() error {
	if strings.TrimSpace(r.GPUType) == "" {
		return errors.New("profile selection: gpu.type is required: the simulated accelerator comes from the environment configuration and cannot be inferred")
	}
	if r.GPUCount <= 0 {
		return fmt.Errorf("profile selection: gpu.count must be greater than 0, got %d: the requested device quantity comes from the workload", r.GPUCount)
	}
	if r.TensorParallel < 0 {
		return fmt.Errorf("profile selection: execution.tensorParallel must not be negative, got %d", r.TensorParallel)
	}
	if r.PipelineParallel < 0 {
		return fmt.Errorf("profile selection: execution.pipelineParallel must not be negative, got %d", r.PipelineParallel)
	}
	if r.PipelineParallel > 1 {
		return fmt.Errorf("profile selection: execution.pipelineParallel %d is not supported: pipeline-parallel execution is rejected, not approximated", r.PipelineParallel)
	}
	if r.TensorParallel > 0 && r.TensorParallel != r.GPUCount {
		return fmt.Errorf("profile selection: execution.tensorParallel %d is inconsistent with gpu.count %d: the recorded topology must match the requested device quantity",
			r.TensorParallel, r.GPUCount)
	}
	if r.BlockSize < 0 {
		return fmt.Errorf("profile selection: cache.blockSize must not be negative, got %d", r.BlockSize)
	}
	return nil
}

// describe renders requirements for error messages without leaking anything
// beyond the workload's own compatibility settings.
func (r Requirements) describe() string {
	revision := r.ModelRevision
	if revision == "" {
		revision = "unpinned"
	}
	requested := ""
	if r.ProfileID != "" {
		requested = fmt.Sprintf(" (profile %q requested by the namespace)", r.ProfileID)
	}
	return fmt.Sprintf("model %q (revision %q) on %s x%d%s", r.Model, revision, r.GPUType, r.GPUCount, requested)
}

// matchRequirements evaluates every compatibility criterion and returns the
// reasons that failed plus the number of criteria the candidate satisfied. The
// count orders candidate diagnostics by closeness.
func matchRequirements(m *Metadata, req Requirements) (reasons []string, score int) {
	check := func(ok bool, format string, args ...any) {
		if ok {
			score++
			return
		}
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	if req.ProfileID != "" {
		check(m.ID == req.ProfileID,
			"profile.id %q does not match the requested profile %q", m.ID, req.ProfileID)
	}

	check(m.Model.Name == req.Model,
		"model.name %q does not match required %q", m.Model.Name, req.Model)

	if req.ModelRevision != "" {
		check(m.Model.Revision == req.ModelRevision,
			"model.revision %q does not match pinned revision %q", m.Model.Revision, req.ModelRevision)
	}

	if req.Tokenizer != "" {
		check(m.Model.Tokenizer == req.Tokenizer,
			"model.tokenizer %q does not match pinned tokenizer %q", m.Model.Tokenizer, req.Tokenizer)
	}
	if req.TokenizerRevision != "" {
		check(m.Model.TokenizerRevision == req.TokenizerRevision,
			"model.tokenizerRevision %q does not match pinned tokenizer revision %q", m.Model.TokenizerRevision, req.TokenizerRevision)
	}
	if req.ChatTemplateHash != "" {
		check(m.Model.ChatTemplateHash == req.ChatTemplateHash,
			"model.chatTemplateHash %q does not match pinned chat template %q", m.Model.ChatTemplateHash, req.ChatTemplateHash)
	}

	check(m.GPU.Type == req.GPUType,
		"gpu.type %q does not match required %q", m.GPU.Type, req.GPUType)

	check(m.GPU.Count == req.GPUCount,
		"gpu.count %d does not match requested %d device(s)", m.GPU.Count, req.GPUCount)

	if req.TensorParallel > 0 {
		check(m.Execution.TensorParallel == req.TensorParallel,
			"execution.tensorParallel %d does not match requested %d", m.Execution.TensorParallel, req.TensorParallel)
	}

	if req.DType != "" {
		check(m.Execution.DType == req.DType,
			"execution.dtype %q does not match required %q", m.Execution.DType, req.DType)
	}

	// An empty quantization value means unquantized, so it is always compared:
	// an unquantized workload must not be served by a quantized recording.
	check(m.Execution.Quantization == req.Quantization,
		"execution.quantization %q does not match required %q (an empty value means unquantized)",
		m.Execution.Quantization, req.Quantization)

	if req.BlockSize > 0 {
		check(m.Cache.BlockSize == req.BlockSize,
			"cache.blockSize %d does not match required %d", m.Cache.BlockSize, req.BlockSize)
	}

	if req.EnablePrefixCaching != nil {
		check(m.Cache.EnablePrefixCaching == *req.EnablePrefixCaching,
			"cache.enablePrefixCaching %t does not match required %t", m.Cache.EnablePrefixCaching, *req.EnablePrefixCaching)
	}

	check(vllmVersionCompatible(m.VLLM.Version, names.SupportedVLLMVersion),
		"vllm.version %q is not the supported release %q: recorded timing is only valid for the release it was captured with",
		m.VLLM.Version, names.SupportedVLLMVersion)

	return reasons, score
}

// vllmVersionCompatible reports whether a profile was recorded against the
// operator's fixed frontend release. Anything else cannot be proven compatible,
// so it is rejected rather than approximated.
func vllmVersionCompatible(profileVersion, supported string) bool {
	normalize := func(v string) string {
		v = strings.TrimSpace(v)
		return strings.TrimPrefix(v, "v")
	}
	return normalize(profileVersion) == normalize(supported)
}
