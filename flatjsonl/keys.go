package flatjsonl

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/cespare/xxhash/v2"
)

type flKey struct {
	path             []string
	isZero           bool
	t                Type
	tt               []Type
	original         string
	canonical        string
	replaced         string
	transposeDst     string
	transposeKey     intOrString
	transposeTrimmed string
	extractors       []extractor
	parent           uint64

	// transposeParents holds ancestor array indexes (outermost first) for
	// keys matched by a nested transpose prefix (with [] segments), e.g. the
	// shipment index for ".shipments.[].cells". Together with transposeKey
	// (the own index) it identifies a transposed row within a source line,
	// so two-level array order can be restored from the transposed details.
	transposeParents []intOrString
}

// transposeHasParent is true for keys matched by a nested transpose prefix.
func (k flKey) transposeHasParent() bool {
	return len(k.transposeParents) > 0
}

// transposeParentKey is the index of the immediate parent array element.
// It must only be called if transposeHasParent is true.
func (k flKey) transposeParentKey() intOrString {
	return k.transposeParents[len(k.transposeParents)-1]
}

// transposeRowKey identifies a row of a transposed table within one source
// line: ancestor array indexes (for nested transpose) and the own array
// index/object property.
func (k flKey) transposeRowKey() string {
	switch len(k.transposeParents) {
	case 0:
		return k.transposeKey.String()
	case 1:
		return k.transposeParents[0].String() + "\x1f" + k.transposeKey.String()
	default:
		var sb strings.Builder

		for _, p := range k.transposeParents {
			sb.WriteString(p.String())
			sb.WriteByte('\x1f')
		}

		sb.WriteString(k.transposeKey.String())

		return sb.String()
	}
}

func (k *flKey) UpdateType(u Type) {
	if len(k.tt) == 0 {
		k.tt = append(k.tt, u)
	} else {
		for _, t := range k.tt {
			if t != u {
				k.tt = append(k.tt, u)

				break
			}
		}
	}

	k.t = k.t.Update(u)
}

type intOrString struct {
	t Type
	i int
	s string
}

func (is intOrString) Value() Value {
	if is.t == TypeString {
		return Value{
			Type:   TypeString,
			String: is.s,
		}
	}

	return Value{
		Type:   TypeFloat,
		Number: float64(is.i),
	}
}

func (is intOrString) String() string {
	if is.s != "" {
		return is.s
	}

	return strconv.Itoa(is.i)
}

func (p *Processor) initKey(pk, parent uint64, path []string, t Type, isZero bool) flKey {
	k, ok := p.flKeys.Load(pk)
	if ok {
		return k
	}

	pp := make([]string, len(path))
	copy(pp, path)

	key := KeyFromPath(path)

	k.UpdateType(t)
	k.isZero = k.isZero && isZero
	k.path = pp
	k.original = key
	k.canonical = p.ck(key)
	k.parent = parent

	p.matchTranspose(&k)

	for r, x := range p.extractRegex {
		if r.MatchString(key) {
			k.extractors = append(k.extractors, x...)

			break
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	existing, ok := p.flKeys.Load(pk)
	if ok {
		return existing
	}

	if p.f.ChildrenLimitObject > 0 && len(k.path) > 1 {
		p.collectKeyCardinality(k)
	}

	if _, ok := p.canonicalKeys[k.canonical]; !ok {
		p.flKeysList = append(p.flKeysList, k.original)
		p.canonicalKeys[k.canonical] = k
	}

	p.flKeys.Store(pk, k)
	atomic.AddInt64(&p.totalKeys, 1)

	return k
}

func (p *Processor) scanKey(pk, parent uint64, path []string, t Type, isZero bool) (_ []extractor, stop bool) {
	if _, phc := p.parentHighCardinality.Load(parent); phc {
		return nil, true
	}

	k, ok := p.flKeys.Load(pk)

	if !ok {
		k = p.initKey(pk, parent, path, t, isZero)
	}

	updType := false

	if k.t != t {
		k.UpdateType(t)

		updType = true
	}

	if updType || (k.isZero && !isZero) {
		p.mu.Lock()
		defer p.mu.Unlock()

		k, _ = p.flKeys.Load(pk)

		k.UpdateType(t)
		k.isZero = k.isZero && isZero

		p.flKeys.Store(pk, k)
	}

	return k.extractors, false
}

func (p *Processor) collectKeyCardinality(k flKey) {
	parentCardinality := p.parentCardinality[k.parent]
	parentCardinality++

	limit := p.f.ChildrenLimitObject

	if p.f.ChildrenLimitArray != 0 && len(k.path) > 1 {
		// Check if last path item has a form of "[123]".
		l := k.path[len(k.path)-1]
		if len(l) > 2 && l[0] == '[' && l[len(l)-1] == ']' {
			isArrayItem := true

			for i := 1; i < len(l)-2; i++ {
				if l[i] < '0' || l[i] > '9' {
					isArrayItem = false

					break
				}
			}

			if isArrayItem {
				limit = p.f.ChildrenLimitArray
			}
		}
	}

	if parentCardinality > limit {
		pp := k.path[0 : len(k.path)-1]
		parentKey := KeyFromPath(pp)
		allowCardinality := false

		for _, ac := range p.cfg.AllowCardinality {
			if parentKey == ac {
				allowCardinality = true
			}
		}

		if !allowCardinality {
			grandParentKey := KeyFromPath(pp[:len(pp)-1])
			ppk, gpk := newHasher().hashParentBytes([]byte(parentKey), len(grandParentKey))

			p.mu.Unlock()
			p.initKey(ppk, gpk, pp, TypeJSON, false)
			p.mu.Lock()

			p.cfg.KeepJSON = append(p.cfg.KeepJSON, parentKey)
			p.parentHighCardinality.Store(k.parent, true)
		} else {
			p.parentCardinality[k.parent] = parentCardinality
		}
	} else {
		p.parentCardinality[k.parent] = parentCardinality
	}
}

// transposeWildcard is a transpose prefix segment that matches any single
// array index segment (e.g. "[0]") and captures it as the parent index.
const transposeWildcard = "[]"

// transposePrefix is a parsed entry of the transpose config map.
type transposePrefix struct {
	segments []string
	dst      string
}

// parseTransposePrefixes prepares transpose config for deterministic matching:
// prefixes with more segments (more specific) come first, ties are broken by
// the joined prefix string.
func parseTransposePrefixes(transpose map[string]string) []transposePrefix {
	res := make([]transposePrefix, 0, len(transpose))

	for tk, dst := range transpose {
		res = append(res, transposePrefix{
			segments: strings.Split(strings.TrimPrefix(tk, "."), "."),
			dst:      dst,
		})
	}

	sort.Slice(res, func(i, j int) bool {
		if len(res[i].segments) != len(res[j].segments) {
			return len(res[i].segments) > len(res[j].segments)
		}

		return strings.Join(res[i].segments, ".") < strings.Join(res[j].segments, ".")
	})

	return res
}

// matchTranspose assigns transpose destination to the key if it matches one
// of the configured transpose prefixes.
func (p *Processor) matchTranspose(k *flKey) {
	for _, tp := range p.transposePrefixes {
		if tp.match(k) {
			return
		}
	}
}

// match checks if the key path is under the transpose prefix and, if so,
// fills transpose properties of the key. A "[]" prefix segment matches any
// single array index segment and captures it as an ancestor index.
func (tp transposePrefix) match(k *flKey) bool {
	path := k.path

	// Key must be strictly longer than the prefix to have a transpose key.
	if len(path) <= len(tp.segments) {
		return false
	}

	var parents []intOrString

	for i, seg := range tp.segments {
		if seg == transposeWildcard {
			idx, ok := parseArrayIndex(path[i])
			if !ok {
				return false
			}

			parents = append(parents, intOrString{t: TypeInt, i: idx})

			continue
		}

		if seg != path[i] {
			return false
		}
	}

	rest := path[len(tp.segments):]

	// The first remaining segment is the transpose key: array index or object property.
	if idx, ok := parseArrayIndex(rest[0]); ok {
		k.transposeKey = intOrString{t: TypeInt, i: idx}
	} else {
		k.transposeKey = intOrString{t: TypeString, s: rest[0]}
	}

	trimmed := "._value"
	if len(rest) > 1 {
		trimmed = "." + strings.Join(rest[1:], ".")
	}

	k.transposeDst = tp.dst
	k.transposeTrimmed = trimmed
	k.transposeParents = parents

	return true
}

// parseArrayIndex parses a path segment of form "[123]" into an array index.
func parseArrayIndex(seg string) (int, bool) {
	if len(seg) < 3 || seg[0] != '[' || seg[len(seg)-1] != ']' {
		return 0, false
	}

	i, err := strconv.Atoi(seg[1 : len(seg)-1])
	if err != nil || i < 0 {
		return 0, false
	}

	return i, true
}

type hasher struct {
	digest *xxhash.Digest
}

func newHasher() *hasher {
	return &hasher{
		digest: xxhash.New(),
	}
}

func (h hasher) hashBytes(flatPath []byte) uint64 {
	if len(flatPath) == 0 {
		return 0
	}

	h.digest.Reset()

	_, err := h.digest.Write(flatPath)
	if err != nil {
		panic("hashing failed: " + err.Error())
	}

	return h.digest.Sum64()
}

// hashParentBytes takes flat path to element as a parent path and last segment.
// It returns hashes for parent path and for full path.
func (h hasher) hashParentBytes(flatPath []byte, parentLen int) (pk uint64, par uint64) {
	h.digest.Reset()

	p1 := flatPath[:parentLen]
	p2 := flatPath[parentLen:]

	if len(p1) == 0 {
		par = 0
	} else {
		_, err := h.digest.Write(p1)
		if err != nil {
			panic("hashing failed: " + err.Error())
		}

		par = h.digest.Sum64()
	}

	if len(p2) != 0 {
		_, err := h.digest.Write(p2)
		if err != nil {
			panic("hashing failed: " + err.Error())
		}
	} else {
		return par, par
	}

	return h.digest.Sum64(), par
}

func (p *Processor) scanAvailableKeys() error {
	p.Log("scanning keys...")
	atomic.StoreInt64(&p.rd.Sequence, 0)

	if p.f.MaxLines > 0 {
		p.rd.MaxLines = int64(p.f.MaxLines)
	}

	if p.f.MaxLinesKeys > 0 && (p.rd.MaxLines == 0 || p.f.MaxLinesKeys < int(p.rd.MaxLines)) {
		p.rd.MaxLines = int64(p.f.MaxLinesKeys)
	}

	p.rd.OffsetLines = int64(p.f.OffsetLines)

	for _, input := range p.inputs {
		err := func() error {
			task := "scanning keys"
			if len(p.inputs) > 1 && input.FileName != "" {
				task = "scanning keys (" + input.FileName + ")"
			}

			sess, err := p.rd.session(input, task)
			if err != nil {
				if errors.Is(err, errEmptyFile) {
					return nil
				}

				return err
			}

			defer sess.Close()

			sess.setupWalker = func(w *FastWalker) {
				h := newHasher()

				w.WantPath = true

				w.FnObjectStop = func(_ int64, flatPath []byte, pl int, path []string) (stop bool) {
					// Nothing to do with empty path.
					if len(flatPath) == 0 {
						return false
					}

					pk, parent := h.hashParentBytes(flatPath, pl)

					_, stop = p.scanKey(pk, parent, path, TypeObject, false)

					return stop
				}

				w.FnArrayStop = func(_ int64, flatPath []byte, pl int, path []string) (stop bool) {
					if len(flatPath) == 0 {
						return
					}

					pk, parent := h.hashParentBytes(flatPath, pl)

					_, stop = p.scanKey(pk, parent, path, TypeArray, false)

					return stop
				}
				w.FnString = func(_ int64, flatPath []byte, pl int, path []string, value []byte) []extractor {
					pk, parent := h.hashParentBytes(flatPath, pl)

					x, _ := p.scanKey(pk, parent, path, TypeString, len(value) == 0)

					return x
				}
				w.FnNumber = func(_ int64, flatPath []byte, pl int, path []string, value float64, _ []byte) {
					pk, parent := h.hashParentBytes(flatPath, pl)
					isInt := float64(int(value)) == value

					if isInt {
						p.scanKey(pk, parent, path, TypeInt, value == 0)
					} else {
						p.scanKey(pk, parent, path, TypeFloat, value == 0)
					}
				}
				w.FnBool = func(_ int64, flatPath []byte, pl int, path []string, value bool) {
					pk, parent := h.hashParentBytes(flatPath, pl)
					p.scanKey(pk, parent, path, TypeBool, !value)
				}
				w.FnNull = func(_ int64, flatPath []byte, pl int, path []string) {
					pk, parent := h.hashParentBytes(flatPath, pl)
					p.scanKey(pk, parent, path, TypeNull, true)
				}
			}

			err = p.rd.Read(sess)
			if err != nil {
				return fmt.Errorf("failed to read: %w", err)
			}

			return nil
		}()
		if err != nil {
			return err
		}
	}

	p.prepareScannedKeys()
	p.iterateIncludeKeys()

	return nil
}

func (p *Processor) prepareScannedKeys() {
	var jsonKeys map[string]bool
	if len(p.cfg.KeepJSON) > 0 {
		jsonKeys = make(map[string]bool)
		for _, key := range p.cfg.KeepJSON {
			jsonKeys[key] = true
		}
	}

	var (
		deleted = map[string]bool{}
		hcOrig  []string
	)

	p.parentHighCardinality.Range(func(key uint64, value bool) bool {
		k, ok := p.flKeys.Load(key)
		if !ok {
			println("BUG: high cardinality key not found")

			return true
		}

		hcOrig = append(hcOrig, k.original)
		k.t = TypeJSON

		p.flKeys.Store(key, k)

		return true
	})

	p.flKeys.Range(func(key uint64, k flKey) bool {
		if k.t == TypeObject || k.t == TypeArray {
			deleted[k.original] = true

			return true
		}

		for _, hc := range hcOrig {
			if k.original == hc {
				if p.f.ShowJSONSchema {
					p.jsonSchema.AddKey(k, p.flKeys)
				}
			}

			if len(k.original) > len(hc) && strings.HasPrefix(k.original, hc) {
				p.flKeys.Delete(key)

				deleted[k.original] = true

				return true
			}
		}

		if p.f.ShowKeysHier {
			p.keyHierarchy.Add(k.path)
		}

		if p.f.ShowJSONSchema {
			p.jsonSchema.AddKey(k, p.flKeys)
		}

		return true
	})

	var newFlKeys []string

	for _, key := range p.flKeysList {
		if !deleted[key] {
			newFlKeys = append(newFlKeys, key)
		}
	}

	p.flKeysList = newFlKeys
}

func (p *Processor) flKeysInit() {
	if p.flKeys.Size() == 0 && len(p.includeKeys) > 0 {
		h := newHasher()

		for key := range p.includeKeys {
			if strings.HasPrefix(key, "const:") {
				continue
			}

			path := strings.Split(strings.TrimPrefix(key, "."), ".")
			flatPath := []byte(key)
			pk := h.hashBytes(flatPath)

			k := flKey{
				path:      path,
				isZero:    false,
				t:         TypeString,
				original:  key,
				canonical: p.ck(key),
			}

			for r, x := range p.extractRegex {
				if r.MatchString(key) {
					k.extractors = append(k.extractors, x...)

					break
				}
			}

			p.flKeys.Store(pk, k)
		}
	}

	p.flKeys.Range(func(_ uint64, value flKey) bool {
		if _, phc := p.parentHighCardinality.Load(value.parent); phc {
			// Skip keys with high cardinality parents.
			return true
		}

		v := p.canonicalKeys[value.canonical]
		value.isZero = value.isZero && v.isZero
		value.t = v.t.Update(value.t)

		p.canonicalKeys[value.canonical] = value

		return true
	})

	for _, k := range p.flKeysList {
		if len(p.cfg.Transpose) > 0 {
			for tk := range p.cfg.Transpose {
				if strings.HasPrefix(k, tk) {
					break
				}
			}
		}
	}
}

func (p *Processor) iterateIncludeKeys() {
	excludeKeys := make(map[string]bool, len(p.cfg.ExcludeKeys))
	for _, k := range p.cfg.ExcludeKeys {
		excludeKeys[p.ck(k)] = true
	}

	i := 0

	for _, k := range p.cfg.IncludeKeys {
		if excludeKeys[p.ck(k)] {
			continue
		}

		exclude := false

		for _, r := range p.excludeRegex {
			if r.MatchString(k) {
				exclude = true

				break
			}
		}

		if exclude {
			continue
		}

		p.addIncludeKey(k, &i)
	}

	p.flKeysInit()

	canonicalIncludes := make(map[string]bool)

	for _, k := range p.flKeysList {
		if _, ok := p.includeKeys[k]; ok {
			continue
		}

		ck := p.ck(k)

		if excludeKeys[ck] {
			continue
		}

		exclude := false

		for _, r := range p.excludeRegex {
			if r.MatchString(k) {
				exclude = true

				break
			}
		}

		if exclude {
			continue
		}

		if canonicalIncludes[ck] {
			continue
		}

		if len(p.includeRegex) > 0 {
			for _, r := range p.includeRegex {
				if r.MatchString(k) {
					canonicalIncludes[k] = true

					p.addIncludeKey(k, &i)

					break
				}
			}
		} else if len(p.cfg.IncludeKeys) == 0 {
			if !p.f.SkipZeroCols {
				canonicalIncludes[k] = true

				p.addIncludeKey(k, &i)
			} else if !p.canonicalKeys[ck].isZero {
				canonicalIncludes[k] = true

				p.addIncludeKey(k, &i)
			}
		}
	}
}

func (p *Processor) addIncludeKey(k string, i *int) {
	if _, found := p.includeKeys[k]; found {
		return
	}

	p.includeKeys[k] = *i
	*i++
}

func (p *Processor) prepareKeys() {
	p.keys = make([]flKey, len(p.includeKeys))

	p.replaceKeys = make(map[string]string)
	p.replaceByKey = make(map[string]string)

	for k, r := range p.cfg.ReplaceKeys {
		mk := p.ck(k)

		p.replaceByKey[r] = mk
		p.replaceKeys[mk] = r
	}

	type ik struct {
		orig string
		idx  int
	}

	sorted := make([]ik, 0, len(p.includeKeys))
	for origKey, i := range p.includeKeys {
		sorted = append(sorted, ik{orig: origKey, idx: i})
	}
	// Sorting by index first to have deterministic order.
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].idx < sorted[j].idx
	})

	// Sorting to put shorter keys first to have better replaces.
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i].orig) < len(sorted[j].orig)
	})

	for _, v := range sorted {
		origKey := v.orig

		ck, ok := p.canonicalKeys[p.ck(origKey)]

		switch {
		case !ok: // Can happen for meta keys like `const:X`.
			ck.replaced = p.prepareKey(origKey)
		case ck.transposeDst == "":
			ck.replaced = p.prepareKey(ck.original)
		default:
			ck.replaced = p.prepareKey(ck.transposeTrimmed)
		}

		p.keys[v.idx] = ck
	}

	keys := make([]flKey, 0, len(p.keys))
	keyExists := make(map[string]int)
	keyMap := make(map[int]int)

	for i, pk := range p.keys {
		if pk.transposeDst == "" {
			if j, ok := keyExists[pk.replaced]; ok {
				pk.UpdateType(p.keys[i].t)
				p.keys[i] = pk
				keyMap[i] = j

				continue
			}

			keyExists[pk.replaced] = len(keys)
		}

		keyMap[i] = len(keys)
		keys = append(keys, pk)
	}

	if len(keyMap) > 0 {
		for k, i := range p.includeKeys {
			if j, ok := keyMap[i]; ok {
				p.includeKeys[k] = j

				if strings.HasPrefix(k, "const:") {
					p.constVals[j] = strings.TrimPrefix(k, "const:")
				}
			}
		}
	}

	p.keys = keys
}

func (p *Processor) prepareKey(origKey string) (kk string) {
	ck := p.ck(origKey)

	defer func() {
		if p.f.StripKeys {
			kk = strings.TrimSpace(kk)
			kk = strings.Fields(kk)[0]
		}

		if p.f.KeyLimit > 0 && len(kk) > p.f.KeyLimit {
			i := p.includeKeys[kk]
			is := strconv.Itoa(i)

			kk = kk[0:(p.f.KeyLimit-len(is))] + is

			p.replaceKeys[ck] = kk
		}
	}()

	if rep, ok := p.replaceKeys[ck]; ok {
		return rep
	}

	for reg, rep := range p.replaceRegex {
		kr := origKey

		matches := reg.FindStringSubmatch(origKey)
		if matches != nil {
			kr = rep

			for i, m := range matches {
				if i == 0 {
					continue
				}

				kr = strings.ReplaceAll(kr, "${"+strconv.Itoa(i)+"}", trimSpaces.ReplaceAllString(strings.TrimSpace(m), "_"))
			}
		}

		if kr != origKey {
			if strings.HasSuffix(kr, "|to_snake_case") {
				kr = toSnakeCase(strings.TrimSuffix(kr, "|to_snake_case"))
			}

			return kr
		}
	}

	if !p.f.ReplaceKeys {
		return origKey
	}

	sk := strings.Split(strings.TrimRight(origKey, "."), ".")
	i := len(sk) - 1
	ski := toSnakeCase(sk[i])
	snk := strings.Trim(ski, "[]")

	for {
		if len(snk) == 0 {
			panic("BUG: empty snk for '" + origKey + "'")
		}

		if stored, ok := p.replaceByKey[snk]; (!ok || origKey == stored) && (snk[0] == '_' || unicode.IsLetter(rune(snk[0]))) {
			p.replaceByKey[snk] = origKey
			origKey = snk

			break
		}

		i--

		if i == 0 {
			break
		}

		ski := toSnakeCase(sk[i])
		snk = strings.Trim(ski, "[]") + "_" + snk
	}

	return origKey
}

var (
	matchFirstCap        = regexp.MustCompile("(.)([A-Z][a-z]+)")
	matchAllCap          = regexp.MustCompile("([a-z0-9])([A-Z])")
	matchNonAlphaNumeric = regexp.MustCompile(`[^a-z0-9A-Z\s()]+`)
)

func toSnakeCase(str string) string {
	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	snake = matchNonAlphaNumeric.ReplaceAllString(snake, "_")

	return strings.ToLower(strings.Trim(strings.ReplaceAll(snake, "_ ", " "), "_"))
}
