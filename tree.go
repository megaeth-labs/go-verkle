// This is free and unencumbered software released into the public domain.
//
// Anyone is free to copy, modify, publish, use, compile, sell, or
// distribute this software, either in source code form or as a compiled
// binary, for any purpose, commercial or non-commercial, and by any
// means.
//
// In jurisdictions that recognize copyright laws, the author or authors
// of this software dedicate any and all copyright interest in the
// software to the public domain. We make this dedication for the benefit
// of the public at large and to the detriment of our heirs and
// successors. We intend this dedication to be an overt act of
// relinquishment in perpetuity of all present and future rights to this
// software under copyright law.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
// IN NO EVENT SHALL THE AUTHORS BE LIABLE FOR ANY CLAIM, DAMAGES OR
// OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE,
// ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR
// OTHER DEALINGS IN THE SOFTWARE.
//
// For more information, please refer to <https://unlicense.org>

package verkle

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/protolambda/go-kzg/bls"
)

// FlushableNode is a tuple of a node and its hash, to be passed
// to a consumer (e.g. the routine responsible for saving it to
// the db) once the node is no longer used by the tree.
type FlushableNode struct {
	Hash [32]byte
	Node VerkleNode
}

type VerkleNode interface {
	// Insert or Update value `v` at key `k`
	Insert(k []byte, v []byte) error

	// Insert "à la" Stacktrie. Same thing as insert, except that
	// values are expected to be ordered, and the commitments and
	// hashes for each subtrie are computed online, as soon as it
	// is clear that no more values will be inserted in there.
	InsertOrdered([]byte, []byte, chan FlushableNode) error

	// Adds an account to the tree
	CreateAccount([]byte, uint64, uint64, uint64, *big.Int, common.Hash) error

	// Get value at key `k`
	Get(k []byte) ([]byte, error)

	// Hash of the current node
	Hash() common.Hash

	// ComputeCommitment computes the commitment of the node
	ComputeCommitment() *bls.G1Point

	// GetCommitment retrieves the (previously computed)
	// commitment of a node.
	GetCommitment() *bls.G1Point

	// GetCommitmentAlongPath follows the path that one key
	// traces through the tree, and collects the various
	// elements needed to build a proof. The order of elements
	// is from the bottom of the tree, up to the root.
	GetCommitmentsAlongPath([]byte) ([]*bls.G1Point, []*bls.Fr, []*bls.Fr, [][]bls.Fr)

	// Serialize encodes the node to RLP.
	Serialize() ([]byte, error)
}

const (
	// Threshold for using multi exponentiation when
	// computing commitment. Number refers to non-zero
	// children in a node.
	multiExpThreshold = 110
)

var (
	errInsertIntoHash  = errors.New("trying to insert into hashed node")
	errValueNotPresent = errors.New("value not present in tree")

	zeroHash = common.HexToHash("0000000000000000000000000000000000000000000000000000000000000000")
)

type (
	// Represents an internal node at any level
	InternalNode struct {
		// List of child nodes of this internal node.
		children []VerkleNode

		// node depth in the tree, in bits
		depth int

		// Cache the hash of the current node
		hash common.Hash

		// Cache the commitment value
		commitment *bls.G1Point

		treeConfig *TreeConfig
	}

	hashedNode struct {
		hash       common.Hash
		commitment *bls.G1Point
	}

	accountLeaf struct {
		leafNode

		Version  uint64
		Balance  *big.Int
		Nonce    uint64
		CodeSize uint64
		CodeHash common.Hash

		commitment *bls.G1Point

		treeConfig *TreeConfig
	}

	leafNode struct {
		key   []byte
		value []byte
	}

	empty struct{}
)

func newInternalNode(depth int, tc *TreeConfig) VerkleNode {
	node := new(InternalNode)
	node.children = make([]VerkleNode, tc.nodeWidth)
	for idx := range node.children {
		node.children[idx] = empty(struct{}{})
	}
	node.depth = depth
	node.treeConfig = tc
	return node
}

// New creates a new tree root
func New(width int) VerkleNode {
	return newInternalNode(0, GetTreeConfig(width))
}

// offset2Key extracts the n bits of a key that correspond to the
// index of a child node.
func offset2Key(key []byte, offset, width int) uint {
	switch width {
	case 10:
		return offset2KeyTenBits(key, offset)
	case 8:
		return uint(key[offset/8])
	default:
		// no need to bother with other width
		// until this is required.
		panic("node width not supported")
	}
}

func offset2KeyTenBits(key []byte, offset int) uint {
	// The node has 1024 children, i.e. 10 bits. Extract it
	// from the key to figure out which child to recurse into.
	// The number is necessarily spread across 2 bytes because
	// the pitch is 10 and therefore a multiple of 2. Hence, no
	// 3 byte scenario is possible.
	nFirstByte := offset / 8
	nBitsInSecondByte := (offset + 10) % 8
	firstBitShift := (8 - (offset % 8))
	lastBitShift := (8 - nBitsInSecondByte) % 8
	leftMask := (key[nFirstByte] >> firstBitShift) << firstBitShift
	ret := (uint(key[nFirstByte]^leftMask) << ((uint(nBitsInSecondByte)-1)%8 + 1))
	if int(nFirstByte)+1 < len(key) {
		// Note that, at the last level, the last 4 bits are
		// zeroed-out so children are 16 bits apart.
		ret |= uint(key[nFirstByte+1] >> lastBitShift)
	}
	return ret
}

func (n *InternalNode) Insert(key []byte, value []byte) error {
	// Clear cached commitment on modification
	if n.commitment != nil {
		n.commitment = nil
	}

	nChild := offset2Key(key, n.depth, n.treeConfig.width)

	switch child := n.children[nChild].(type) {
	case empty:
		n.children[nChild] = &leafNode{key: key, value: value}
	case *hashedNode:
		return errInsertIntoHash
	case *leafNode:
		// Need to add a new branch node to differentiate
		// between two keys, if the keys are different.
		// Otherwise, just update the key.
		if bytes.Equal(child.key, key) {
			child.value = value
		} else {
			width := n.treeConfig.width

			// A new branch node has to be inserted. Depending
			// on the next word in both keys, a recursion into
			// the moved leaf node can occur.
			nextWordInExistingKey := offset2Key(child.key, n.depth+n.treeConfig.width, n.treeConfig.width)
			newBranch := newInternalNode(n.depth+width, n.treeConfig).(*InternalNode)
			n.children[nChild] = newBranch
			newBranch.children[nextWordInExistingKey] = child

			nextWordInInsertedKey := offset2Key(key, n.depth+width, width)
			if nextWordInInsertedKey != nextWordInExistingKey {
				// Next word differs, so this was the last level.
				// Insert it directly into its final slot.
				newBranch.children[nextWordInInsertedKey] = &leafNode{key: key, value: value}
			} else {
				newBranch.Insert(key, value)
			}
		}
	default: // InternalNode
		return child.Insert(key, value)
	}
	return nil
}

func (n *InternalNode) InsertOrdered(key []byte, value []byte, flush chan FlushableNode) error {
	// Clear cached commitment on modification
	if n.commitment != nil {
		n.commitment = nil
	}

	nChild := offset2Key(key, n.depth, n.treeConfig.width)

	switch child := n.children[nChild].(type) {
	case empty:
		// Insert into a new subtrie, which means that the
		// subtree directly preceding this new one, can
		// safely be calculated.
		for i := int(nChild) - 1; i >= 0; i-- {
			switch n.children[i].(type) {
			case empty:
				continue
			case *leafNode:
				childHash := n.children[i].Hash()
				if flush != nil {
					flush <- FlushableNode{childHash, n.children[i]}
				}
				n.children[i] = &hashedNode{hash: childHash}
				break
			case *hashedNode:
				break
			default:
				comm := n.children[i].ComputeCommitment()
				// Doesn't re-compute commitment as it's cached
				h := n.children[i].Hash()
				if flush != nil {
					n.children[i].(*InternalNode).Flush(flush)
				}
				n.children[i] = &hashedNode{hash: h, commitment: comm}
				break
			}
		}

		n.children[nChild] = &leafNode{key: key, value: value}
	case *hashedNode:
		return errInsertIntoHash
	case *leafNode:
		// Need to add a new branch node to differentiate
		// between two keys, if the keys are different.
		// Otherwise, just update the key.
		if bytes.Equal(child.key, key) {
			child.value = value
		} else {
			width := n.treeConfig.width

			// A new branch node has to be inserted. Depending
			// on the next word in both keys, a recursion into
			// the moved leaf node can occur.
			nextWordInExistingKey := offset2Key(child.key, n.depth+width, width)
			newBranch := newInternalNode(n.depth+width, n.treeConfig).(*InternalNode)
			n.children[nChild] = newBranch

			nextWordInInsertedKey := offset2Key(key, n.depth+width, width)
			if nextWordInInsertedKey != nextWordInExistingKey {
				// Directly hash the (left) node that was already
				// inserted.
				h := child.Hash()
				comm := new(bls.G1Point)
				var tmp bls.Fr
				hashToFr(&tmp, h, n.treeConfig.modulus)
				bls.MulG1(comm, &bls.GenG1, &tmp)
				if flush != nil {
					flush <- FlushableNode{h, child}
				}
				newBranch.children[nextWordInExistingKey] = &hashedNode{hash: h, commitment: comm}
				// Next word differs, so this was the last level.
				// Insert it directly into its final slot.
				newBranch.children[nextWordInInsertedKey] = &leafNode{key: key, value: value}
			} else {
				// Reinsert the leaf in order to recurse
				newBranch.children[nextWordInExistingKey] = child
				newBranch.InsertOrdered(key, value, flush)
			}
		}
	default: // InternalNode
		return child.InsertOrdered(key, value, flush)
	}
	return nil
}

func (n *InternalNode) CreateAccount(key []byte, version, nonce, codeSize uint64, balance *big.Int, codeHash common.Hash) error {
	nChild := offset2Key(key, n.depth, n.treeConfig.width)

	switch child := n.children[nChild].(type) {
	case *leafNode, *hashedNode, nil:
		return errors.New("trying to create an account in an invalid subtree")
	case empty:
		n.children[nChild] = &accountLeaf{
			leafNode: leafNode{key: key},
			Version:  version,
			Balance:  balance,
			Nonce:    nonce,
			CodeSize: codeSize,
			CodeHash: codeHash,
		}
		return nil
	case *accountLeaf:
		// Insert an intermediate node
		newBranch := newInternalNode(n.depth+n.treeConfig.width, n.treeConfig).(*InternalNode)

		nExisting := offset2Key(child.key, n.depth+1, n.treeConfig.width)
		nNew := offset2Key(key, n.depth+1, n.treeConfig.width)

		newBranch.children[nExisting] = child

		if nExisting != nNew {
			// Both the current account node and the inserted
			// one share the same key segment. Introduce an
			// intermediate node and recurse.
			n.children[nChild] = newBranch
			return newBranch.CreateAccount(key, version, nonce, codeSize, balance, codeHash)
		}

		// The new branch is the first differing branch, set both of them
		// in their own slot.
		n.children[nNew] = new(accountLeaf)
		return n.children[nNew].CreateAccount(key, version, nonce, codeSize, balance, codeHash)
	default:
		return child.CreateAccount(key, version, nonce, codeSize, balance, codeHash)
	}
}

// Flush hashes the children of an internal node and replaces them
// with hashedNode. It also sends the current node on the flush channel.
func (n *InternalNode) Flush(flush chan FlushableNode) {
	for i, child := range n.children {
		if c, ok := child.(*InternalNode); ok {
			c.Flush(flush)
			n.children[i] = &hashedNode{c.Hash(), c.commitment}
		} else if c, ok := child.(*leafNode); ok {
			childHash := c.Hash()
			flush <- FlushableNode{childHash, c}
			n.children[i] = &hashedNode{hash: childHash}
		}
	}
	flush <- FlushableNode{n.Hash(), n}
}

func (n *InternalNode) Get(k []byte) ([]byte, error) {
	nChild := offset2Key(k, n.depth, n.treeConfig.width)

	switch child := n.children[nChild].(type) {
	case empty, *hashedNode, nil:
		return nil, errors.New("trying to read from an invalid child")
	default:
		return child.Get(k)
	}
}

func (n *InternalNode) Hash() common.Hash {
	comm := n.ComputeCommitment()
	h := sha256.Sum256(bls.ToCompressedG1(comm))
	return common.BytesToHash(h[:])
}

// This function takes a hash and turns it into a bls.Fr integer, making
// sure that this doesn't overflow the modulus.
// This piece of code is really ugly, and probably a performance hog, it
// needs to be rewritten more efficiently.
func hashToFr(out *bls.Fr, h [32]byte, modulus *big.Int) {
	var h2 [32]byte
	// reverse endianness
	for i := range h {
		h2[i] = h[len(h)-i-1]
	}

	// Apply modulus
	x := big.NewInt(0).SetBytes(h2[:])
	x.Mod(x, modulus)

	// clear the buffer in case the trailing bytes were 0
	for i := 0; i < 32; i++ {
		h2[i] = 0
	}
	copy(h2[32-len(x.Bytes()):], x.Bytes())

	// back to original endianness
	for i := range h2 {
		h[i] = h2[len(h)-i-1]
	}

	if !bls.FrFrom32(out, h) {
		panic(fmt.Sprintf("invalid Fr number %x", h))
	}
}

func (n *InternalNode) ComputeCommitment() *bls.G1Point {
	if n.commitment != nil {
		return n.commitment
	}

	emptyChildren := 0
	poly := make([]bls.Fr, n.treeConfig.nodeWidth)
	for idx, childC := range n.children {
		switch child := childC.(type) {
		case empty:
			emptyChildren++
		case *leafNode, *hashedNode:
			hashToFr(&poly[idx], child.Hash(), n.treeConfig.modulus)
		default:
			compressed := bls.ToCompressedG1(childC.ComputeCommitment())
			hashToFr(&poly[idx], sha256.Sum256(compressed), n.treeConfig.modulus)
		}
	}

	var commP *bls.G1Point
	if n.treeConfig.nodeWidth-emptyChildren >= multiExpThreshold {
		commP = bls.LinCombG1(n.treeConfig.lg1, poly[:])
	} else {
		var comm bls.G1Point
		bls.CopyG1(&comm, &bls.ZERO_G1)
		for i := range poly {
			if !bls.EqualZero(&poly[i]) {
				var tmpG1, eval bls.G1Point
				bls.MulG1(&eval, &n.treeConfig.lg1[i], &poly[i])
				bls.CopyG1(&tmpG1, &comm)
				bls.AddG1(&comm, &tmpG1, &eval)
			}
		}
		commP = &comm
	}
	n.commitment = commP
	return n.commitment
}

func (n *InternalNode) GetCommitment() *bls.G1Point {
	return n.commitment
}

func (n *InternalNode) GetCommitmentsAlongPath(key []byte) ([]*bls.G1Point, []*bls.Fr, []*bls.Fr, [][]bls.Fr) {
	childIdx := offset2Key(key, n.depth, n.treeConfig.width)
	comms, zis, yis, fis := n.children[childIdx].GetCommitmentsAlongPath(key)
	var zi, yi bls.Fr
	bls.AsFr(&zi, uint64(childIdx))
	fi := make([]bls.Fr, n.treeConfig.nodeWidth)
	for i, child := range n.children {
		hashToFr(&fi[i], child.Hash(), n.treeConfig.modulus)
		if i == int(childIdx) {
			bls.CopyFr(&yi, &fi[i])
		}
	}
	return append(comms, n.GetCommitment()), append(zis, &zi), append(yis, &yi), append(fis, fi[:])
}

func (n *InternalNode) Serialize() ([]byte, error) {
	var bitlist [128]uint8
	children := make([]byte, 0, n.treeConfig.nodeWidth*32)
	for i, c := range n.children {
		if _, ok := c.(empty); !ok {
			setBit(bitlist[:], i)
			children = append(children, c.Hash().Bytes()...)
		}
	}
	return rlp.EncodeToBytes([]interface{}{bitlist, children})
}

const (
	accountLeafVersion = iota
	accountLeafBalance
	accountLeafNonce
	accountLeafCodeHash
	accountLeafCodeSize
)

func (n *accountLeaf) Insert(k []byte, value []byte) error {
	// The parent insert is expected to ensure that this
	// situation doesn't occur. This check will catch an
	// invalid situation while the library stabilizes.
	if !bytes.Equal(k[:31], n.key[:31]) {
		return errors.New("inserting invalid key into key")
	}

	switch k[31] {
	case accountLeafBalance:
		n.Balance.SetBytes(value)
		n.commitment = nil // invalidate commitment
	case accountLeafNonce:
		n.Nonce = binary.BigEndian.Uint64(value)
		n.commitment = nil // invalidate commitment
	default:
		return errors.New("writing to read-only location")
	}
	return nil
}

func (n *accountLeaf) CreateAccount(k []byte, version, nonce, codeSize uint64, balance *big.Int, codeHash common.Hash) error {
	return errors.New("creating over an already exisiting account")
}

func (n *accountLeaf) InsertOrdered(key []byte, value []byte, flush chan FlushableNode) error {
	err := n.Insert(key, value)
	if err != nil && flush != nil {
		flush <- FlushableNode{n.Hash(), n}
	}
	return err
}

func (n *accountLeaf) Get(k []byte) ([]byte, error) {
	if !bytes.Equal(k, n.key) {
		return nil, errValueNotPresent
	}

	switch k[31] {
	case accountLeafVersion:
		var ret [8]byte
		binary.BigEndian.PutUint64(ret[:], n.Version)
		return ret[:], nil
	case accountLeafBalance:
		return n.Balance.Bytes(), nil
	case accountLeafNonce:
		var ret [8]byte
		binary.BigEndian.PutUint64(ret[:], n.Nonce)
		return ret[:], nil
	case accountLeafCodeHash:
		return n.CodeHash[:], nil
	case accountLeafCodeSize:
		var ret [8]byte
		binary.BigEndian.PutUint64(ret[:], n.CodeSize)
		return ret[:], nil
	default:
		return nil, errValueNotPresent
	}
}

func (n *accountLeaf) ComputeCommitment() *bls.G1Point {
	// TODO only allocate if the thing isn't already
	// allocated. Otherwise, just overwrite it.
	n.commitment = new(bls.G1Point)
	bls.CopyG1(n.commitment, &bls.ZERO_G1)

	// Build the polynomial based on the account information
	var poly [5]bls.Fr
	bls.AsFr(&poly[0], n.Version)
	var data [32]byte
	n.Balance.FillBytes(data[:])
	hashToFr(&poly[1], data, n.treeConfig.modulus)
	bls.AsFr(&poly[2], n.Nonce)
	hashToFr(&poly[3], n.CodeHash, n.treeConfig.modulus)
	bls.AsFr(&poly[4], n.CodeSize)

	for i := range poly {
		if !bls.EqualZero(&poly[i]) {
			var eval bls.G1Point
			bls.MulG1(&eval, &n.treeConfig.lg1[i], &poly[i])
			bls.AddG1(n.commitment, n.commitment, &eval)
		}
	}

	return n.commitment
}

func (n *accountLeaf) GetCommitment() *bls.G1Point {
	return n.commitment
}

func (n *accountLeaf) GetCommitmentsAlongPath(key []byte) ([]*bls.G1Point, []*bls.Fr, []*bls.Fr, [][]bls.Fr) {
	return nil, nil, nil, nil
}

func (n *accountLeaf) Hash() common.Hash {
	h := sha256.Sum256(bls.ToCompressedG1(n.ComputeCommitment()))
	return common.BytesToHash(h[:])
}

func (n *accountLeaf) Serialize() ([]byte, error) {
	return rlp.EncodeToBytes(struct {
		k []byte
		v uint64
		b *big.Int
		m uint64
		h []byte
		s uint64
	}{n.key, n.Version, n.Balance, n.Nonce, n.CodeHash[:], n.CodeSize})
}

func (n *leafNode) Insert(k []byte, value []byte) error {
	// The parent insert is expected to ensure that this
	// situation doesn't occur. This check will catch an
	// invalid situation while the library stabilizes.
	if !bytes.Equal(k, n.key) {
		return errors.New("inserting invalid key into key")
	}

	n.key = k
	n.value = value
	return nil
}

func (n *leafNode) InsertOrdered(key []byte, value []byte, flush chan FlushableNode) error {
	err := n.Insert(key, value)
	if err != nil && flush != nil {
		flush <- FlushableNode{n.Hash(), n}
	}
	return err
}

func (n *leafNode) CreateAccount(key []byte, version, nonce, codeSize uint64, balance *big.Int, codeHash common.Hash) error {
	return errors.New("inserting an account node in a storage leaf")
}

func (n *leafNode) Get(k []byte) ([]byte, error) {
	if !bytes.Equal(k, n.key) {
		return nil, errValueNotPresent
	}
	return n.value, nil
}

func (n *leafNode) ComputeCommitment() *bls.G1Point {
	panic("can't compute the commitment directly")
}

func (n *leafNode) GetCommitment() *bls.G1Point {
	panic("can't get the commitment directly")
}

func (n *leafNode) GetCommitmentsAlongPath(key []byte) ([]*bls.G1Point, []*bls.Fr, []*bls.Fr, [][]bls.Fr) {
	return nil, nil, nil, nil
}

func (n *leafNode) Hash() common.Hash {
	digest := sha256.New()
	digest.Write(n.key)
	digest.Write(n.value)
	return common.BytesToHash(digest.Sum(nil))
}

func (n *leafNode) Serialize() ([]byte, error) {
	return rlp.EncodeToBytes([][]byte{n.key, n.value})
}

func (n *hashedNode) Insert(k []byte, value []byte) error {
	return errInsertIntoHash
}

func (n *hashedNode) InsertOrdered(key []byte, value []byte, _ chan FlushableNode) error {
	return errInsertIntoHash
}

func (n *hashedNode) CreateAccount(key []byte, version, nonce, codeSize uint64, balance *big.Int, codeHash common.Hash) error {
	return errors.New("inserting an account node in a hash node")
}

func (n *hashedNode) Get(k []byte) ([]byte, error) {
	return nil, errors.New("can not read from a hash node")
}

func (n *hashedNode) Hash() common.Hash {
	return n.hash
}

func (n *hashedNode) ComputeCommitment() *bls.G1Point {
	if n.commitment == nil {
		var hashAsFr bls.Fr
		hashToFr(&hashAsFr, n.hash, big.NewInt(0))
		n.commitment = new(bls.G1Point)
		bls.MulG1(n.commitment, &bls.GenG1, &hashAsFr)
	}
	return n.commitment
}

func (n *hashedNode) GetCommitment() *bls.G1Point {
	return n.commitment
}

func (n *hashedNode) GetCommitmentsAlongPath(key []byte) ([]*bls.G1Point, []*bls.Fr, []*bls.Fr, [][]bls.Fr) {
	panic("can not get the full path, and there is no proof of absence")
}

func (n *hashedNode) Serialize() ([]byte, error) {
	return rlp.EncodeToBytes([][]byte{n.hash[:]})
}

func (e empty) Insert(k []byte, value []byte) error {
	return errors.New("an empty node should not be inserted directly into")
}

func (e empty) InsertOrdered(key []byte, value []byte, _ chan FlushableNode) error {
	return e.Insert(key, value)
}

func (e empty) CreateAccount(key []byte, version, nonce, codeSize uint64, balance *big.Int, codeHash common.Hash) error {
	return errors.New("inserting an account node directly into an empty node")
}

func (e empty) Get(k []byte) ([]byte, error) {
	return nil, nil
}

func (e empty) Hash() common.Hash {
	return zeroHash
}

func (e empty) ComputeCommitment() *bls.G1Point {
	return &bls.ZeroG1
}

func (e empty) GetCommitment() *bls.G1Point {
	return &bls.ZeroG1
}

func (e empty) GetCommitmentsAlongPath(key []byte) ([]*bls.G1Point, []*bls.Fr, []*bls.Fr, [][]bls.Fr) {
	panic("trying to produce a commitment for an empty subtree")
}

func (e empty) Serialize() ([]byte, error) {
	return nil, errors.New("can't encode empty node to RLP")
}

func setBit(bitlist []uint8, index int) {
	byt := index / 8
	bit := index % 8
	bitlist[byt] |= (uint8(1) << bit)
}
