// Package bbitest builds tiny valid UCSC BBI files (bigWig / bigBed) for tests —
// one chromosome, a single uncompressed data block, a single R-tree leaf. It lets
// varhub test the bigwig/bigbed source formats end-to-end without external kent
// tools. It is test-support only.
package bbitest

import (
	"encoding/binary"
	"math"
	"os"
)

const (
	magicBigWig  = 0x888FFC26
	magicBigBed  = 0x8789F2EB
	magicChromBP = 0x78CA8C91
	magicCIRTree = 0x2468ACE0
)

// WigItem is one bigWig interval and its value.
type WigItem struct {
	Start, End uint32
	Val        float32
}

// BedItem is one bigBed interval; Rest is the tab-joined remaining columns
// (e.g. "name\tscore").
type BedItem struct {
	Start, End uint32
	Rest       string
}

// WriteBigWig writes a bigWig file with one bedGraph section over chrom.
func WriteBigWig(path, chrom string, items []WigItem) error {
	le := binary.LittleEndian
	var minS, maxE uint32 = math.MaxUint32, 0
	for _, it := range items {
		if it.Start < minS {
			minS = it.Start
		}
		if it.End > maxE {
			maxE = it.End
		}
	}
	blk := make([]byte, 24)
	le.PutUint32(blk[4:], minS)
	le.PutUint32(blk[8:], maxE)
	blk[20] = 1 // bedGraph
	le.PutUint16(blk[22:], uint16(len(items)))
	for _, it := range items {
		row := make([]byte, 12)
		le.PutUint32(row[0:], it.Start)
		le.PutUint32(row[4:], it.End)
		le.PutUint32(row[8:], math.Float32bits(it.Val))
		blk = append(blk, row...)
	}
	return os.WriteFile(path, build(magicBigWig, chrom, blk, minS, maxE), 0o644)
}

// WriteBigBed writes a bigBed file over chrom.
func WriteBigBed(path, chrom string, items []BedItem) error {
	le := binary.LittleEndian
	var minS, maxE uint32 = math.MaxUint32, 0
	var blk []byte
	for _, it := range items {
		if it.Start < minS {
			minS = it.Start
		}
		if it.End > maxE {
			maxE = it.End
		}
		row := make([]byte, 12)
		le.PutUint32(row[4:], it.Start)
		le.PutUint32(row[8:], it.End)
		blk = append(blk, row...)
		blk = append(blk, []byte(it.Rest)...)
		blk = append(blk, 0)
	}
	return os.WriteFile(path, build(magicBigBed, chrom, blk, minS, maxE), 0o644)
}

func build(magic uint32, chrom string, dataBlock []byte, spanStart, spanEnd uint32) []byte {
	le := binary.LittleEndian
	keySize := uint32(len(chrom))

	chromTree := make([]byte, 32)
	le.PutUint32(chromTree[0:], magicChromBP)
	le.PutUint32(chromTree[4:], 1)
	le.PutUint32(chromTree[8:], keySize)
	le.PutUint32(chromTree[12:], 8)
	le.PutUint64(chromTree[16:], 1)
	node := make([]byte, 4)
	node[0] = 1
	le.PutUint16(node[2:], 1)
	item := make([]byte, keySize+8)
	copy(item, chrom)
	le.PutUint32(item[keySize:], 0)
	le.PutUint32(item[keySize+4:], 100000)
	chromTree = append(chromTree, append(node, item...)...)

	const chromTreeOff = 64
	dataOff := uint64(chromTreeOff + len(chromTree))
	indexOff := dataOff + uint64(len(dataBlock))

	rtree := make([]byte, 48)
	le.PutUint32(rtree[0:], magicCIRTree)
	le.PutUint32(rtree[4:], 1)
	le.PutUint64(rtree[8:], 1)
	le.PutUint32(rtree[20:], spanStart)
	le.PutUint32(rtree[28:], spanEnd)
	le.PutUint64(rtree[32:], indexOff)
	le.PutUint32(rtree[40:], 1)
	rn := make([]byte, 4)
	rn[0] = 1
	le.PutUint16(rn[2:], 1)
	rit := make([]byte, 32)
	le.PutUint32(rit[4:], spanStart)
	le.PutUint32(rit[12:], spanEnd)
	le.PutUint64(rit[16:], dataOff)
	le.PutUint64(rit[24:], uint64(len(dataBlock)))
	rtree = append(rtree, append(rn, rit...)...)

	hdr := make([]byte, 64)
	le.PutUint32(hdr[0:], magic)
	le.PutUint16(hdr[4:], 4)
	le.PutUint64(hdr[8:], chromTreeOff)
	le.PutUint64(hdr[16:], dataOff)
	le.PutUint64(hdr[24:], indexOff)

	out := append([]byte{}, hdr...)
	out = append(out, chromTree...)
	out = append(out, dataBlock...)
	out = append(out, rtree...)
	return out
}
