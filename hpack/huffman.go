package hpack

import (
	"bytes"
	"fmt"
)

var huffmanTree *HuffmanTreeNode

func init() {
	huffmanTree = genHuffmanTree()
}

func HuffmanDecoder(bs []byte) (string, error) {
	var buf bytes.Buffer

	offset := 0
	curDepth := 0
	node := huffmanTree
	for offset < len(bs) {
		char := bs[offset]
		for curBit := 7; curBit >= 0; curBit-- {
			right := ((char >> curBit) & 1) == 1
			if right {
				node = node.right
			} else {
				node = node.left
			}
			curDepth++

			if node.value != nil {
				if *node.value == eosByte {
					return "", fmt.Errorf("7 bit padding exceeded, found EOS")
				}
				buf.WriteByte(byte(*node.value))
				node = huffmanTree
				curDepth = 0
			}
		}
		offset++
	}

	if node != huffmanTree {
		if curDepth > 7 {
			return "", fmt.Errorf("incomplete encoding")
		}

		for node.right != nil {
			node = node.right
		}

		if node.value == nil || *node.value != 256 {
			return "", fmt.Errorf("incomplete encoding")
		}
	}

	return buf.String(), nil
}

type HuffmanTreeNode struct {
	left  *HuffmanTreeNode
	right *HuffmanTreeNode
	value *int
}

func intptr(i int) *int {
	return &i
}

func genHuffmanTree() *HuffmanTreeNode {
	root := &HuffmanTreeNode{}

	for char, enc := range huffmanCodings {
		bits := enc.bits

		current := root
		for i := 0; i < enc.n; i++ {
			right := ((bits >> (enc.n - i - 1)) & 1) == 1

			if right {
				if current.right == nil {
					current.right = &HuffmanTreeNode{}
				}
				current = current.right
			} else {
				if current.left == nil {
					current.left = &HuffmanTreeNode{}
				}
				current = current.left
			}
		}
		// avoid go for-loop weirdness
		current.value = intptr(char)
	}

	return root
}
