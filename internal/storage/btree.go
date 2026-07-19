package storage

import "fmt"

// btreeDegree t: each non-root node has between t-1 and 2t-1 keys.
// t=4 → max 7 keys per node, max 8 children per internal node.
const btreeDegree = 4

// BTree is a B+ tree: all data lives in leaf nodes.
// Internal nodes hold separator keys only.
// Leaf nodes are linked for efficient forward range scans.
type BTree struct {
	root btNode
	Size int // total key-tuple pairs inserted
}

type btNode interface {
	btIsFull() bool
	btIsLeaf() bool
}

type btLeaf struct {
	keys []interface{}
	vals [][]Tuple // vals[i] = all tuples with keys[i] (handles duplicates)
	next *btLeaf
}

type btInternal struct {
	keys     []interface{}
	children []btNode
}

func (n *btLeaf) btIsFull() bool     { return len(n.keys) >= 2*btreeDegree-1 }
func (n *btInternal) btIsFull() bool { return len(n.keys) >= 2*btreeDegree-1 }
func (n *btLeaf) btIsLeaf() bool     { return true }
func (n *btInternal) btIsLeaf() bool { return false }

// NewBTree returns an empty B+ tree.
func NewBTree() *BTree {
	return &BTree{root: &btLeaf{}}
}

// cmpKeys compares two index key values. Numeric types and strings supported.
func cmpKeys(a, b interface{}) int {
	af, aok := toNumKey(a)
	bf, bok := toNumKey(b)
	if aok && bok {
		if af < bf {
			return -1
		}
		if af > bf {
			return 1
		}
		return 0
	}
	as := fmt.Sprintf("%v", a)
	bs := fmt.Sprintf("%v", b)
	if as < bs {
		return -1
	}
	if as > bs {
		return 1
	}
	return 0
}

func toNumKey(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// leafPos returns the index where key is or would be inserted in keys.
// Returns (index, true) if exact match found, (index, false) for insertion point.
func leafPos(keys []interface{}, key interface{}) (int, bool) {
	lo, hi := 0, len(keys)
	for lo < hi {
		mid := (lo + hi) / 2
		c := cmpKeys(keys[mid], key)
		if c == 0 {
			return mid, true
		} else if c < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, false
}

// childIdx returns the child index to descend into for a given key.
func childIdx(keys []interface{}, key interface{}) int {
	i := len(keys) - 1
	for i >= 0 && cmpKeys(keys[i], key) > 0 {
		i--
	}
	return i + 1
}

// Insert adds (key, tuple) into the B+ tree.
func (t *BTree) Insert(key interface{}, tuple Tuple) {
	if t.root.btIsFull() {
		newRoot := &btInternal{children: []btNode{t.root}}
		t.splitChild(newRoot, 0)
		t.root = newRoot
	}
	t.insertNonFull(t.root, key, tuple)
	t.Size++
}

func (t *BTree) insertNonFull(node btNode, key interface{}, tuple Tuple) {
	if leaf, ok := node.(*btLeaf); ok {
		i, found := leafPos(leaf.keys, key)
		if found {
			leaf.vals[i] = append(leaf.vals[i], tuple)
			return
		}
		leaf.keys = append(leaf.keys, nil)
		copy(leaf.keys[i+1:], leaf.keys[i:])
		leaf.keys[i] = key
		leaf.vals = append(leaf.vals, nil)
		copy(leaf.vals[i+1:], leaf.vals[i:])
		leaf.vals[i] = []Tuple{tuple}
		return
	}
	internal := node.(*btInternal)
	i := childIdx(internal.keys, key)
	child := internal.children[i]
	if child.btIsFull() {
		t.splitChild(internal, i)
		if cmpKeys(key, internal.keys[i]) >= 0 {
			i++
		}
	}
	t.insertNonFull(internal.children[i], key, tuple)
}

// splitChild splits internal.children[ci] which must be full.
func (t *BTree) splitChild(parent *btInternal, ci int) {
	child := parent.children[ci]

	if leaf, ok := child.(*btLeaf); ok {
		// B+ tree leaf split: right sibling gets upper half, separator key is first key of right sibling.
		sp := btreeDegree // split point: left keeps [0:sp], right gets [sp:]
		newLeaf := &btLeaf{
			keys: append([]interface{}{}, leaf.keys[sp:]...),
			vals: append([][]Tuple{}, leaf.vals[sp:]...),
			next: leaf.next,
		}
		leaf.keys = leaf.keys[:sp]
		leaf.vals = leaf.vals[:sp]
		leaf.next = newLeaf

		promotedKey := newLeaf.keys[0]
		parent.keys = append(parent.keys, nil)
		copy(parent.keys[ci+1:], parent.keys[ci:])
		parent.keys[ci] = promotedKey
		parent.children = append(parent.children, nil)
		copy(parent.children[ci+2:], parent.children[ci+1:])
		parent.children[ci+1] = newLeaf
		return
	}

	// Internal node split: middle key is promoted up and removed from child.
	internal := child.(*btInternal)
	mid := btreeDegree - 1
	promotedKey := internal.keys[mid]
	newInternal := &btInternal{
		keys:     append([]interface{}{}, internal.keys[mid+1:]...),
		children: append([]btNode{}, internal.children[mid+1:]...),
	}
	internal.keys = internal.keys[:mid]
	internal.children = internal.children[:mid+1]

	parent.keys = append(parent.keys, nil)
	copy(parent.keys[ci+1:], parent.keys[ci:])
	parent.keys[ci] = promotedKey
	parent.children = append(parent.children, nil)
	copy(parent.children[ci+2:], parent.children[ci+1:])
	parent.children[ci+1] = newInternal
}

// findLeaf descends to the leaf node that contains (or would contain) key.
func (t *BTree) findLeaf(key interface{}) *btLeaf {
	node := t.root
	for !node.btIsLeaf() {
		internal := node.(*btInternal)
		i := childIdx(internal.keys, key)
		node = internal.children[i]
	}
	return node.(*btLeaf)
}

// Search returns all tuples with the exact key.
func (t *BTree) Search(key interface{}) []Tuple {
	leaf := t.findLeaf(key)
	i, found := leafPos(leaf.keys, key)
	if !found {
		return nil
	}
	result := make([]Tuple, len(leaf.vals[i]))
	copy(result, leaf.vals[i])
	return result
}

// RangeScan returns tuples satisfying: lo loOp key hiOp hi.
// loOp: ">" | ">=" | "=" | "" (no lower bound).
// hiOp: "<" | "<=" | "=" | "" (no upper bound).
// When loOp == "=" this is an exact lookup (hi is ignored).
func (t *BTree) RangeScan(lo interface{}, loOp string, hi interface{}, hiOp string) []Tuple {
	if loOp == "=" {
		return t.Search(lo)
	}

	var startLeaf *btLeaf
	var startPos int

	if lo != nil {
		startLeaf = t.findLeaf(lo)
		i, found := leafPos(startLeaf.keys, lo)
		if loOp == ">" && found {
			startPos = i + 1
		} else {
			startPos = i
		}
	} else {
		// Start from leftmost leaf.
		node := t.root
		for !node.btIsLeaf() {
			node = node.(*btInternal).children[0]
		}
		startLeaf = node.(*btLeaf)
		startPos = 0
	}

	var results []Tuple
	for leaf := startLeaf; leaf != nil; leaf = leaf.next {
		start := 0
		if leaf == startLeaf {
			start = startPos
		}
		for i := start; i < len(leaf.keys); i++ {
			key := leaf.keys[i]
			if hi != nil {
				c := cmpKeys(key, hi)
				if hiOp == "<" && c >= 0 {
					return results
				}
				if hiOp == "<=" && c > 0 {
					return results
				}
			}
			results = append(results, leaf.vals[i]...)
		}
	}
	return results
}

// All returns all tuples in ascending key order.
func (t *BTree) All() []Tuple {
	return t.RangeScan(nil, "", nil, "")
}

// Depth returns the height of the tree (root = 1).
func (t *BTree) Depth() int {
	depth := 1
	node := t.root
	for !node.btIsLeaf() {
		depth++
		node = node.(*btInternal).children[0]
	}
	return depth
}
