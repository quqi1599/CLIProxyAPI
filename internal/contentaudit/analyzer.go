package contentaudit

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/go-ego/gse"
	"golang.org/x/text/unicode/norm"
)

const moderationKeywordSeedFrequency = 60_000

//go:embed data/zh_s.txt
var moderationChineseDictionary string

// moderationAnalyzer tokenizes policy phrases and request text through the same
// immutable, keyword-seeded dictionary. A compact candidate scan runs before
// this analyzer so ordinary requests without any policy substring do not pay
// the segmentation cost.
type moderationAnalyzer struct {
	segmenter *gse.Segmenter
}

var moderationAnalyzerCache struct {
	sync.Mutex
	key      string
	analyzer *moderationAnalyzer
}

func buildModerationAnalyzer(keywords []string) (*moderationAnalyzer, error) {
	seeds := collectHanKeywordSeeds(keywords)
	cacheKey := moderationSeedSetHash(seeds)

	moderationAnalyzerCache.Lock()
	defer moderationAnalyzerCache.Unlock()
	if moderationAnalyzerCache.key == cacheKey && moderationAnalyzerCache.analyzer != nil {
		return moderationAnalyzerCache.analyzer, nil
	}

	segmenter := &gse.Segmenter{
		AlphaNum:   true,
		NotLoadHMM: true,
		SkipLog:    true,
	}
	if err := segmenter.LoadDictStr(moderationChineseDictionary); err != nil {
		return nil, err
	}
	for _, seed := range seeds {
		if _, _, exists := segmenter.Find(seed); exists {
			if err := segmenter.ReAddToken(seed, moderationKeywordSeedFrequency); err != nil {
				return nil, err
			}
			continue
		}
		if err := segmenter.AddToken(seed, moderationKeywordSeedFrequency); err != nil {
			return nil, err
		}
	}
	segmenter.CalcToken()

	analyzer := &moderationAnalyzer{segmenter: segmenter}
	moderationAnalyzerCache.key = cacheKey
	moderationAnalyzerCache.analyzer = analyzer
	return analyzer, nil
}

func moderationSeedSetHash(seeds []string) string {
	hasher := sha256.New()
	for _, seed := range seeds {
		_, _ = hasher.Write([]byte(seed))
		_, _ = hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func collectHanKeywordSeeds(keywords []string) []string {
	seen := make(map[string]struct{})
	for _, keyword := range keywords {
		// Managed policies may persist segmented Han keywords with spaces.
		// Canonicalizing policy entries does not authorize compacting requests.
		filtered := filterModerationVariantText(keyword)
		var run strings.Builder
		flush := func() {
			seed := run.String()
			run.Reset()
			if len([]rune(seed)) < 2 {
				return
			}
			seen[seed] = struct{}{}
		}
		for _, character := range filtered {
			if unicode.Is(unicode.Han, character) {
				run.WriteRune(character)
				continue
			}
			flush()
		}
		flush()
	}
	seeds := make([]string, 0, len(seen))
	for seed := range seen {
		seeds = append(seeds, seed)
	}
	sort.Strings(seeds)
	return seeds
}

func (a *moderationAnalyzer) normalize(value string) string {
	return a.normalizeFiltered(filterModerationText(value))
}

func (a *moderationAnalyzer) normalizeVariant(value string) string {
	return a.normalizeFiltered(filterModerationVariantText(value))
}

func (a *moderationAnalyzer) normalizeFiltered(filtered string) string {
	if a == nil || a.segmenter == nil {
		return ""
	}
	if filtered == "" {
		return ""
	}
	var output strings.Builder
	output.Grow(len(filtered))
	for index, fragment := range strings.Split(filtered, "\n") {
		if index > 0 {
			output.WriteByte('\n')
		}
		first := true
		for _, token := range a.segmenter.Cut(fragment, false) {
			token = normalizeModerationToken(token)
			if token == "" {
				continue
			}
			if !first {
				output.WriteByte(' ')
			}
			output.WriteString(token)
			first = false
		}
	}
	return strings.TrimSpace(output.String())
}

func normalizeModerationToken(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			output.WriteRune(unicode.ToLower(character))
		}
	}
	return output.String()
}

// filterModerationText preserves structural boundaries for local enforcement.
// In particular, punctuation, newlines, invisible separators and spaces between
// Han characters cannot manufacture a word or move intent across clauses.
func filterModerationText(value string) string {
	value = strings.ToLower(norm.NFKC.String(value))
	var output strings.Builder
	output.Grow(len(value))
	var previous rune
	pendingSpace, pendingBoundary := false, false
	for _, character := range value {
		if isModerationInvisible(character) || unicode.IsPunct(character) || unicode.IsSymbol(character) || character == '\n' || character == '\r' || unicode.IsControl(character) {
			pendingBoundary = output.Len() > 0
			continue
		}
		if unicode.IsSpace(character) {
			pendingSpace = output.Len() > 0
			continue
		}
		if pendingBoundary || pendingSpace && unicode.Is(unicode.Han, previous) && unicode.Is(unicode.Han, character) {
			output.WriteByte('\n')
		} else if pendingSpace {
			output.WriteByte(' ')
		}
		pendingSpace, pendingBoundary = false, false
		output.WriteRune(character)
		previous = character
	}
	return strings.TrimSpace(output.String())
}

// filterModerationVariantText is a recall-only view. Its matches must never
// become a local block without an independently supported original-text match.
func filterModerationVariantText(value string) string {
	value = strings.ToLower(norm.NFKC.String(value))
	var output strings.Builder
	output.Grow(len(value))
	var previous rune
	hasPrevious := false
	pendingSeparator := false
	for _, character := range value {
		if isModerationInvisible(character) {
			continue
		}
		if unicode.IsSpace(character) || unicode.IsPunct(character) || unicode.IsSymbol(character) {
			pendingSeparator = hasPrevious
			continue
		}
		if pendingSeparator && !(unicode.Is(unicode.Han, previous) && unicode.Is(unicode.Han, character)) {
			output.WriteByte(' ')
		}
		output.WriteRune(character)
		previous = character
		hasPrevious = true
		pendingSeparator = false
	}
	return strings.TrimSpace(output.String())
}

func moderationCandidateText(value string) string {
	value = strings.ToLower(norm.NFKC.String(value))
	var output strings.Builder
	output.Grow(len(value))
	for _, character := range value {
		if isModerationInvisible(character) {
			continue
		}
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}

func isModerationInvisible(character rune) bool {
	return unicode.Is(unicode.Cf, character) ||
		unicode.Is(unicode.Mn, character) ||
		unicode.Is(unicode.Mc, character) ||
		unicode.Is(unicode.Me, character)
}
