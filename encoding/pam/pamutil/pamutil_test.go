package pamutil_test

import (
	"testing"

	"github.com/Schaudge/grailbio/biopb"
	gbam "github.com/Schaudge/grailbio/encoding/bam"
	"github.com/Schaudge/grailbio/encoding/pam/pamutil"
	"github.com/grailbio/testutil/expect"
)

func TestBlockRangeIntersects(t *testing.T) {
	tests := []struct {
		blockStartRefid, blockStartPos, blockEndRefid, blockEndPos,
		rangeStartRefid, rangeStartPos, rangeLimitRefid, rangeLimitPos int32
		intersect bool
	}{
		{3, 2, 10, 5, 10, 5, 11, 0, true},
		{3, 2, 10, 5, 10, 6, 11, 0, false},
		{3, 2, 10, 5, 3, 0, 3, 2, false},
		{3, 2, 10, 5, 3, 0, 3, 3, true},
	}
	for _, test := range tests {
		blockStart := biopb.Coord{test.blockStartRefid, test.blockStartPos, 0}
		blockEnd := biopb.Coord{test.blockEndRefid, test.blockEndPos, 0}
		r := biopb.CoordRange{biopb.Coord{test.rangeStartRefid, test.rangeStartPos, 0},
			biopb.Coord{test.rangeLimitRefid, test.rangeLimitPos, 0}}
		expect.EQ(t, pamutil.BlockIntersectsRange(blockStart, blockEnd, r), test.intersect, test)
	}
}

func TestRecRangeIntersects(t *testing.T) {
	tests := []struct {
		startRefid0, startPos0, limitRefid0, limitPos0,
		startRefid1, startPos1, limitRefid1, limitPos1 int32
		intersect bool
	}{
		{3, 2, 10, 5, 10, 4, 11, 0, true},
		{3, 2, 10, 5, 10, 5, 11, 0, false},
		{3, 2, 10, 5, 0, 0, 3, 2, false},
		{3, 2, 10, 5, 0, 0, 3, 3, true},
	}
	for _, test := range tests {
		r0 := biopb.CoordRange{biopb.Coord{test.startRefid0, test.startPos0, 0},
			biopb.Coord{test.limitRefid0, test.limitPos0, 0}}
		r1 := biopb.CoordRange{biopb.Coord{test.startRefid1, test.startPos1, 0},
			biopb.Coord{test.limitRefid1, test.limitPos1, 0}}
		expect.EQ(t, test.intersect, r0.Intersects(r1), test)
	}
}

func TestRecRangeContains(t *testing.T) {
	tests := []struct {
		a        biopb.Coord
		contains bool
	}{
		{biopb.Coord{10, 19, 0}, false},
		{biopb.Coord{10, 20, 0}, true},
		{biopb.Coord{12, 0, 0}, true},
		{biopb.Coord{15, 4, 0}, true},
		{biopb.Coord{15, 5, 0}, false},
	}
	r := biopb.CoordRange{biopb.Coord{10, 20, 0}, biopb.Coord{15, 5, 0}}
	for _, test := range tests {
		expect.EQ(t, test.contains, r.Contains(test.a), test)
	}
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		path                         string
		expectedType                 pamutil.FileType
		expectedDir                  string
		expectedStart, expectedLimit biopb.Coord
		expectedField                gbam.FieldType
		expectError                  bool
	}{
		{"foo/0:0,-:0.index", pamutil.FileTypeShardIndex, "foo", biopb.Coord{0, 0, 0}, biopb.Coord{-1, 0, 0}, gbam.FieldInvalid, false},
		{"foo/0:0:10,-:0:11.index", pamutil.FileTypeShardIndex, "foo", biopb.Coord{0, 0, 10}, biopb.Coord{-1, 0, 11}, gbam.FieldInvalid, false},
		{"foo/-:123,-:234.index", pamutil.FileTypeShardIndex, "foo", biopb.Coord{-1, 123, 0}, biopb.Coord{-1, 234, 0}, gbam.FieldInvalid, false},
		{"foo/3:123,4:234.index", pamutil.FileTypeShardIndex, "foo", biopb.Coord{3, 123, 0}, biopb.Coord{4, 234, 0}, gbam.FieldInvalid, false},
		{"foo/3:123,4:234.aux", pamutil.FileTypeFieldData, "foo", biopb.Coord{3, 123, 0}, biopb.Coord{4, 234, 0}, gbam.FieldAux, false},
		{"foo", pamutil.FileTypeUnknown, "", biopb.Coord{0, 0, 0}, biopb.Coord{0, 0, 0}, gbam.FieldInvalid, true},
		{"s3://foo.bar/0:0,-:0.index", pamutil.FileTypeShardIndex, "s3://foo.bar", biopb.Coord{0, 0, 0}, biopb.Coord{-1, 0, 0}, gbam.FieldInvalid, false},
		{"/foo.bar/0:0,-:0.index", pamutil.FileTypeShardIndex, "/foo.bar", biopb.Coord{0, 0, 0}, biopb.Coord{-1, 0, 0}, gbam.FieldInvalid, false},
	}
	for _, test := range tests {
		fi, err := pamutil.ParsePath(test.path)
		if test.expectError {
			expect.NotNil(t, err)
		} else {
			expect.NoError(t, err)
		}
		expect.EQ(t, test.expectedType, fi.Type, "Test", test)
		if fi.Type != pamutil.FileTypeUnknown {
			expect.EQ(t, test.expectedDir, fi.Dir, "Test", test)
		}
		expect.EQ(t, test.expectedStart, fi.Range.Start, "Test", test)
		expect.EQ(t, test.expectedLimit, fi.Range.Limit, "Test", test)
		if test.expectedField != gbam.FieldInvalid {
			expect.EQ(t, test.expectedField.String(), fi.Field, "Test", test)
		}
	}
}

func TestFieldType(t *testing.T) {
	expect.EQ(t, "aux", gbam.FieldAux.String())
	f, err := gbam.ParseFieldType("aux")
	expect.NoError(t, err)
	expect.EQ(t, gbam.FieldAux, f)
	f, err = gbam.ParseFieldType("coord")
	expect.NoError(t, err)
	expect.EQ(t, gbam.FieldCoord, f)
}

func TestCoord(t *testing.T) {
	tests := []struct {
		r0, r1 biopb.Coord
		lt     bool
		ge     bool
	}{
		{biopb.Coord{0, 1, 0}, biopb.Coord{0, 1, 0}, false, true}, // equal
		{biopb.Coord{0, 1, 0}, biopb.Coord{0, 2, 0}, true, false},
		{biopb.Coord{0, 2, 0}, biopb.Coord{0, 1, 0}, false, true},
		{biopb.Coord{0, 2, 0}, biopb.Coord{biopb.InfinityRefID, 2, 0}, true, false},
		{biopb.Coord{0, 2, 0}, biopb.Coord{1, 2, 0}, true, false},
		// {biopb.Coord{0, 709305, 0}, biopb.Coord{0, 44478570, 0}, true, false},
	}
	for _, test := range tests {
		expect.EQ(t, test.lt, test.r0.LT(test.r1), "LT: %+v", test)
		expect.EQ(t, test.ge, test.r0.GE(test.r1), "GE: %+v", test)
	}
}
