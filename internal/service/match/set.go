package match

// set is a compiled ordered list of patterns. The literal starts of the
// patterns are indexed in two tries (exact bytes, and folded bytes for
// the case-insensitive literals): a key is matched only against the
// patterns whose literal it begins with, plus those without a literal.
type set struct {
	ms []*matcher
	// always are the patterns without a literal start (always candidates).
	always []bool
	exact  *trieNode
	folded *trieNode
}

var _ Set = (*set)(nil)

// trieNode is a node of a byte trie; ids are the patterns whose literal
// ends at the node.
type trieNode struct {
	next map[byte]*trieNode
	ids  []int
}

// insert adds the literal of pattern id.
func (n *trieNode) insert(literal string, id int) {
	for i := 0; i < len(literal); i++ {
		if n.next == nil {
			n.next = make(map[byte]*trieNode)
		}
		child, ok := n.next[literal[i]]
		if !ok {
			child = &trieNode{}
			n.next[literal[i]] = child
		}
		n = child
	}
	n.ids = append(n.ids, id)
}

// walk marks the patterns whose literal is a prefix of key.
func (n *trieNode) walk(key string, mark func(id int)) {
	for i := 0; ; i++ {
		for _, id := range n.ids {
			mark(id)
		}
		if i >= len(key) || n.next == nil {
			return
		}
		child, ok := n.next[key[i]]
		if !ok {
			return
		}
		n = child
	}
}

// newSet indexes compiled patterns.
func newSet(ms []*matcher) *set {
	s := &set{ms: ms, always: make([]bool, len(ms))}
	for i, m := range ms {
		switch {
		case m.literal == "":
			s.always[i] = true
		case m.literalFold:
			if s.folded == nil {
				s.folded = &trieNode{}
			}
			s.folded.insert(m.literal, i)
		default:
			if s.exact == nil {
				s.exact = &trieNode{}
			}
			s.exact.insert(m.literal, i)
		}
	}
	return s
}

// Len implements Set.
func (s *set) Len() int { return len(s.ms) }

// Matcher implements Set.
func (s *set) Matcher(i int) Matcher { return s.ms[i] }

// Match implements Set.
func (s *set) Match(key string, keep func(i int) bool, yield func(i int, r Result) bool) {
	var candidates []bool
	folded, haveFolded := "", false
	fold := func() string {
		if !haveFolded {
			folded, haveFolded = foldString(key), true
		}
		return folded
	}
	if s.exact != nil || s.folded != nil {
		candidates = make([]bool, len(s.ms))
		mark := func(id int) { candidates[id] = true }
		if s.exact != nil {
			s.exact.walk(key, mark)
		}
		if s.folded != nil {
			s.folded.walk(fold(), mark)
		}
	}
	for i, m := range s.ms {
		if !s.always[i] && !candidates[i] {
			continue
		}
		if keep != nil && !keep(i) {
			continue
		}
		var r Result
		switch {
		case m.re == nil:
			// A prefix pattern found in the trie matches.
			r = Result{Matched: true}
		case m.need.fold && !m.need.possible(key, fold()):
		case !m.need.fold && !m.need.possible(key, ""):
		default:
			r = m.run(key)
		}
		if r.Matched && !yield(i, r) {
			return
		}
	}
}
