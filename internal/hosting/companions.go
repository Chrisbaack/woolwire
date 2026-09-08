package hosting

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// A GGUF repository holds more than models. Beside the quantizations sit files
// that are useless alone and needed alongside a specific model:
//
//   - a multimodal projector (mmproj-*.gguf), without which a vision model
//     silently loads as a text-only one;
//   - a multi-token-prediction module (mtp-*.gguf), which llama.cpp can run as
//     a speculative draft.
//
// Both are published in several precisions, and which one to take is not a
// choice anyone should have to make: it follows from the model being used.
// They are therefore never offered as models of their own — they are attached
// to the weights they belong to.

// CompanionKind names the role a file plays beside a model.
type CompanionKind string

const (
	// CompanionProjector is a multimodal projector.
	CompanionProjector CompanionKind = "projector"
	// CompanionDraft is a multi-token-prediction module, used as a draft
	// model for speculative decoding.
	CompanionDraft CompanionKind = "draft"
)

var (
	projectorPattern = regexp.MustCompile(`(?i)(^|[-_.])mmproj([-_.]|$)`)
	draftPattern     = regexp.MustCompile(`(?i)^mtp[-_.]`)
)

// classifyCompanion reports what role a GGUF file plays, and "" when it is a
// model in its own right.
func classifyCompanion(relPath string) CompanionKind {
	name := strings.TrimSuffix(path.Base(relPath), ".gguf")
	switch {
	case projectorPattern.MatchString(name):
		return CompanionProjector
	case draftPattern.MatchString(name), strings.EqualFold(path.Base(path.Dir(relPath)), "MTP"):
		return CompanionDraft
	}
	return ""
}

// precisionRank orders the precisions a companion is published in. A projector
// is small enough that precision costs nothing worth optimizing, so the
// conventional F16 is preferred and F32 taken only when it is all there is.
func precisionRank(name string, kind CompanionKind) int {
	upper := strings.ToUpper(name)
	order := []string{"-F16", "-BF16", "-Q8_0", "-F32"}
	if kind == CompanionDraft {
		// A draft model is only a speed optimization, so the cheapest
		// precision that exists is the right default.
		order = []string{"-Q8_0", "-F16", "-BF16", "-F32"}
	}
	for i, suffix := range order {
		if strings.Contains(upper, suffix+".GGUF") || strings.HasSuffix(upper, suffix) {
			return i
		}
	}
	return len(order)
}

// companionCandidate is one file competing to be a model's companion.
type companionCandidate struct {
	Path string
	Size int64
	Kind CompanionKind
}

// chooseCompanions picks one file per role from the candidates found beside a
// model. quantization, when known, lets a draft module published per
// quantization be matched to the model actually being loaded.
func chooseCompanions(candidates []companionCandidate, quantization string) []companionCandidate {
	byKind := map[CompanionKind][]companionCandidate{}
	for _, c := range candidates {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}

	chosen := make([]companionCandidate, 0, 2)
	for _, kind := range []CompanionKind{CompanionProjector, CompanionDraft} {
		group := byKind[kind]
		if len(group) == 0 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			// An exact quantization match beats every other consideration:
			// a draft module built for this quantization is the one meant
			// for it.
			if quantization != "" {
				iMatch := strings.Contains(strings.ToUpper(group[i].Path), "-"+quantization+".")
				jMatch := strings.Contains(strings.ToUpper(group[j].Path), "-"+quantization+".")
				if iMatch != jMatch {
					return iMatch
				}
			}
			iRank, jRank := precisionRank(group[i].Path, kind), precisionRank(group[j].Path, kind)
			if iRank != jRank {
				return iRank < jRank
			}
			if group[i].Size != group[j].Size {
				return group[i].Size < group[j].Size
			}
			return group[i].Path < group[j].Path
		})
		chosen = append(chosen, group[0])
	}
	return chosen
}
