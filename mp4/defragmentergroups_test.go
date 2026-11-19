package mp4_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/go-test/deep"
)

// defragGroupSample returns n samples of 512 ticks each from decode time dts,
// tagged so that the payload differs between fragments.
func defragGroupSample(t *testing.T, w *bytes.Buffer, seqNr, trackID uint32, dts uint64,
	nrSamples int, tag byte, sgpds []*mp4.SgpdBox, sbgps []*mp4.SbgpBox) {
	t.Helper()
	frag, err := mp4.CreateFragment(seqNr, trackID)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]mp4.FullSample, 0, nrSamples)
	for i := 0; i < nrSamples; i++ {
		flags := defragNonSyncFlags()
		if i == 0 {
			flags = defragSyncFlags()
		}
		samples = append(samples, mp4.FullSample{
			Sample:     mp4.Sample{Flags: flags, Dur: 512, Size: 10},
			DecodeTime: dts + uint64(i)*512,
			Data:       defragPayload(tag+byte(i), 10),
		})
	}
	frag.AddFullSamples(samples)
	for _, sgpd := range sgpds {
		if err := frag.Moof.Traf.AddChild(sgpd); err != nil {
			t.Fatal(err)
		}
	}
	for _, sbgp := range sbgps {
		if err := frag.Moof.Traf.AddChild(sbgp); err != nil {
			t.Fatal(err)
		}
	}
	if err := frag.Encode(w); err != nil {
		t.Fatal(err)
	}
}

// trafGroupIndexOffset is the value that a group_description_index in a track
// fragment adds to the 1-based index of its own description box entry
// (ISO/IEC 14496-12 8.9.4).
const trafGroupIndexOffset = 65536

// trafSbgp returns a sample-to-group box in a track fragment that maps
// nrSamples samples to the given fragment-local description index.
func trafSbgp(groupingType string, nrSamples, localIndex uint32) *mp4.SbgpBox {
	return &mp4.SbgpBox{
		GroupingType:            groupingType,
		SampleCounts:            []uint32{nrSamples},
		GroupDescriptionIndices: []uint32{trafGroupIndexOffset + localIndex},
	}
}

// trafSgpdRoll returns a track-fragment description box with one roll entry.
func trafSgpdRoll(rollDistance int16) *mp4.SgpdBox {
	entry := &mp4.RollSampleGroupEntry{RollDistance: rollDistance}
	return &mp4.SgpdBox{
		Version:            1,
		GroupingType:       "roll",
		DefaultLength:      2,
		SampleGroupEntries: []mp4.SampleGroupEntry{entry},
	}
}

// stblGroups returns the sample group boxes of the first track of file.
func stblGroups(t *testing.T, file *mp4.File) (sdtp *mp4.SdtpBox, sbgps []*mp4.SbgpBox, sgpds []*mp4.SgpdBox) {
	t.Helper()
	stbl := file.Moov.Trak.Mdia.Minf.Stbl
	return stbl.Sdtp, stbl.Sbgps, stbl.Sgpds
}

// TestDefragmentMergesFragmentSampleGroups pins that the sample groups of the
// fragments are merged into the progressive sample table with one description
// box per grouping type and each surviving sample pointing at its description.
func TestDefragmentMergesFragmentSampleGroups(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1,
		[]*mp4.SgpdBox{trafSgpdRoll(-1)}, []*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	defragGroupSample(t, &buf, 2, 1, 1024, 2, 11,
		[]*mp4.SgpdBox{trafSgpdRoll(-2)}, []*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	sdtp, sbgps, sgpds := stblGroups(t, outFile)
	if sdtp != nil {
		t.Error("input without dependency information got an sdtp box")
	}
	if len(sgpds) != 1 {
		t.Fatalf("got %d sgpd boxes, want one merged description box", len(sgpds))
	}
	var rolls []int16
	for _, entry := range sgpds[0].SampleGroupEntries {
		rolls = append(rolls, entry.(*mp4.RollSampleGroupEntry).RollDistance)
	}
	if diff := deep.Equal(rolls, []int16{-1, -2}); diff != nil {
		t.Errorf("merged roll descriptions: %v", diff)
	}
	if len(sbgps) != 1 {
		t.Fatalf("got %d sbgp boxes, want one merged sample-to-group box", len(sbgps))
	}
	if diff := deep.Equal(sbgps[0].SampleCounts, []uint32{2, 2}); diff != nil {
		t.Errorf("sample counts: %v", diff)
	}
	if diff := deep.Equal(sbgps[0].GroupDescriptionIndices, []uint32{1, 2}); diff != nil {
		t.Errorf("group description indices: %v", diff)
	}
}

// TestDefragmentDropsUnreferencedSampleGroupDescriptions pins that a
// description that no surviving sample points at is not written.
func TestDefragmentDropsUnreferencedSampleGroupDescriptions(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	unreferenced := &mp4.SgpdBox{
		Version:       1,
		GroupingType:  "roll",
		DefaultLength: 2,
		SampleGroupEntries: []mp4.SampleGroupEntry{
			&mp4.RollSampleGroupEntry{RollDistance: -9},
			&mp4.RollSampleGroupEntry{RollDistance: -1},
		},
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1,
		[]*mp4.SgpdBox{unreferenced}, []*mp4.SbgpBox{trafSbgp("roll", 2, 2)})
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 1 {
		t.Fatalf("got %d sgpd boxes, want only the referenced description", len(sgpds))
	}
	if got := sgpds[0].SampleGroupEntries[0].(*mp4.RollSampleGroupEntry).RollDistance; got != -1 {
		t.Errorf("roll distance %d, want the referenced -1", got)
	}
	if len(sbgps) != 1 || len(sbgps[0].GroupDescriptionIndices) != 1 ||
		sbgps[0].GroupDescriptionIndices[0] != 1 {
		t.Errorf("sbgp %v, want the single kept description as index 1", sbgps)
	}
}

// TestDefragmentDeduplicatesSampleGroupDescriptions pins that equal
// descriptions from different fragments collapse into one entry, so the
// samples of both fragments keep pointing at the same description.
func TestDefragmentDeduplicatesSampleGroupDescriptions(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1,
		[]*mp4.SgpdBox{trafSgpdRoll(-3)}, []*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	defragGroupSample(t, &buf, 2, 1, 1024, 2, 11,
		[]*mp4.SgpdBox{trafSgpdRoll(-3)}, []*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 1 {
		t.Fatalf("got %d sgpd boxes with %d entries, want one entry", len(sgpds), len(sgpds[0].SampleGroupEntries))
	}
	if len(sbgps) != 1 {
		t.Fatalf("got %d sbgp boxes, want one", len(sbgps))
	}
	if diff := deep.Equal(sbgps[0].SampleCounts, []uint32{4}); diff != nil {
		t.Errorf("sample counts: %v", diff)
	}
	if diff := deep.Equal(sbgps[0].GroupDescriptionIndices, []uint32{1}); diff != nil {
		t.Errorf("group description indices: %v", diff)
	}
}

// TestDefragmentSupersededSampleGroupsDisappear pins that the groups of
// superseded samples do not reach the output: only the descriptions of the
// surviving samples are written.
func TestDefragmentSupersededSampleGroupsDisappear(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	twoRolls := &mp4.SgpdBox{
		Version:       1,
		GroupingType:  "roll",
		DefaultLength: 2,
		SampleGroupEntries: []mp4.SampleGroupEntry{
			&mp4.RollSampleGroupEntry{RollDistance: -1},
			&mp4.RollSampleGroupEntry{RollDistance: -9},
		},
	}
	defragGroupSample(t, &buf, 1, 1, 0, 4, 1,
		[]*mp4.SgpdBox{twoRolls}, []*mp4.SbgpBox{trafSbgp("roll", 4, 2)})
	defragGroupSample(t, &buf, 2, 1, 0, 4, 11,
		[]*mp4.SgpdBox{trafSgpdRoll(-5)}, []*mp4.SbgpBox{trafSbgp("roll", 4, 1)})
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 1 {
		t.Fatalf("got %d sgpd boxes, want only the surviving description", len(sgpds))
	}
	entry := sgpds[0].SampleGroupEntries[0].(*mp4.RollSampleGroupEntry)
	if entry.RollDistance != -5 {
		t.Errorf("roll distance %d, want the surviving fragment's -5", entry.RollDistance)
	}
	if len(sbgps) != 1 || len(sbgps[0].SampleCounts) != 1 || sbgps[0].SampleCounts[0] != 4 {
		t.Errorf("sbgp %v, want one run of 4 samples", sbgps)
	}
}

// TestDefragmentTrackSelectionKeepsOnlyKeptTrackGroups pins that the sample
// groups of a dropped track do not reach the output.
func TestDefragmentTrackSelectionKeepsOnlyKeptTrackGroups(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1,
		[]*mp4.SgpdBox{trafSgpdRoll(-1)}, []*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	defragGroupSample(t, &buf, 2, 2, 0, 2, 11,
		[]*mp4.SgpdBox{trafSgpdRoll(-7)}, []*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	out, err := defragment(t, buf.Bytes(), 2)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	if len(outFile.Moov.Traks) != 1 || outFile.Moov.Traks[0].Tkhd.TrackID != 2 {
		t.Fatalf("got %d tracks, want only track 2", len(outFile.Moov.Traks))
	}
	_, _, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 1 {
		t.Fatalf("got %d sgpd boxes, want the kept track's one", len(sgpds))
	}
	if got := sgpds[0].SampleGroupEntries[0].(*mp4.RollSampleGroupEntry).RollDistance; got != -7 {
		t.Errorf("roll distance %d, want the kept track's -7", got)
	}
}

// TestDefragmentKeepsSampleDependencies pins that the leading, dependency and
// redundancy flags of the fragments survive as an sdtp box, one entry per
// surviving sample.
func TestDefragmentKeepsSampleDependencies(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	flags := []uint32{
		mp4.SampleFlags{SampleDependsOn: 2}.Encode(),
		mp4.SampleFlags{IsLeading: 1, SampleDependsOn: 1, SampleIsDependedOn: 1, SampleHasRedundancy: 2}.Encode(),
		mp4.SampleFlags{IsLeading: 2, SampleDependsOn: 1, SampleIsDependedOn: 2, SampleHasRedundancy: 1}.Encode(),
	}
	samples := make([]mp4.FullSample, 0, len(flags))
	for i, f := range flags {
		samples = append(samples, mp4.FullSample{
			Sample:     mp4.Sample{Flags: f, Dur: 512, Size: 10},
			DecodeTime: uint64(i) * 512,
			Data:       defragPayload(byte(i), 10),
		})
	}
	addDefragFragment(t, &buf, 1, 1, samples)
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	sdtp, _, _ := stblGroups(t, outFile)
	if sdtp == nil {
		t.Fatal("dependency information did not survive as an sdtp box")
	}
	if len(sdtp.Entries) != len(flags) {
		t.Fatalf("sdtp has %d entries, want %d", len(sdtp.Entries), len(flags))
	}
	for i, want := range flags {
		sf := mp4.DecodeSampleFlags(want)
		entry := sdtp.Entries[i]
		if entry.IsLeading() != sf.IsLeading || entry.SampleDependsOn() != sf.SampleDependsOn ||
			entry.SampleIsDependedOn() != sf.SampleIsDependedOn ||
			entry.SampleHasRedundancy() != sf.SampleHasRedundancy {
			t.Errorf("sample %d sdtp entry %+v, want %+v", i+1, entry, sf)
		}
	}
}

// TestDefragmentNoSampleGroupsStaysCompatible pins that input without sample
// groups and dependency information keeps the output free of those boxes.
func TestDefragmentNoSampleGroupsStaysCompatible(t *testing.T) {
	data, _, _ := writeDefragTestFile(t)
	out, err := defragment(t, data)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	for _, trak := range outFile.Moov.Traks {
		stbl := trak.Mdia.Minf.Stbl
		if stbl.Sdtp != nil || stbl.Sbgp != nil || len(stbl.Sbgps) > 0 ||
			stbl.Sgpd != nil || len(stbl.Sgpds) > 0 {
			t.Errorf("track %d got sample group or dependency boxes for input without them",
				trak.Tkhd.TrackID)
		}
	}
}

// TestDefragmentKeepsSbgpVersion pins that a sample-to-group box that
// signalled grouping_type_parameter stays version 1 with that parameter, and
// that one that did not stays version 0.
func TestDefragmentKeepsSbgpVersion(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	v1 := &mp4.SbgpBox{
		Version: 1, GroupingTypeParameter: 0, GroupingType: "roll",
		SampleCounts: []uint32{2}, GroupDescriptionIndices: []uint32{trafGroupIndexOffset + 1},
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1, []*mp4.SgpdBox{trafSgpdRoll(-1)}, []*mp4.SbgpBox{v1})
	defragGroupSample(t, &buf, 2, 1, 1024, 2, 11, []*mp4.SgpdBox{trafSgpdRoll(-2)},
		[]*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, _ := stblGroups(t, outFile)
	if len(sbgps) != 1 {
		t.Fatalf("got %d sbgp boxes, want one", len(sbgps))
	}
	if sbgps[0].Version != 1 {
		t.Errorf("sbgp version %d, want 1 since the input signalled grouping_type_parameter",
			sbgps[0].Version)
	}
}

// TestDefragmentGroupSampleCountTooLargeIsError pins that a sample-to-group
// box mapping more samples than its scope holds rejects the input.
func TestDefragmentGroupSampleCountTooLargeIsError(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	sbgp := &mp4.SbgpBox{
		GroupingType: "roll", SampleCounts: []uint32{3},
		GroupDescriptionIndices: []uint32{trafGroupIndexOffset + 1},
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1, []*mp4.SgpdBox{trafSgpdRoll(-1)}, []*mp4.SbgpBox{sbgp})
	_, err := defragment(t, buf.Bytes(), 1)
	if err == nil || !strings.Contains(err.Error(), "maps 3 samples") {
		t.Errorf("oversized sample-to-group mapping error %v", err)
	}
}

// TestDefragmentTrackSgpdDefaultCoversFragmentSamples pins that a fragment
// that maps none of its samples to a grouping type takes the default
// description of the track-level version 2 description box.
func TestDefragmentTrackSgpdDefaultCoversFragmentSamples(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	stbl := init.Moov.Traks[0].Mdia.Minf.Stbl
	stbl.AddChild(&mp4.SgpdBox{
		Version: 2, GroupingType: "roll", DefaultLength: 2, DefaultGroupDescriptionIndex: 1,
		SampleGroupEntries: []mp4.SampleGroupEntry{&mp4.RollSampleGroupEntry{RollDistance: -3}},
	})
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1, nil, nil)
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 1 {
		t.Fatalf("got %d sgpd boxes, want the track default description", len(sgpds))
	}
	if len(sbgps) != 1 || sbgps[0].SampleCounts[0] != 2 {
		t.Errorf("sbgp %v, want both fragment samples mapped to the default", sbgps)
	}
}

// TestDefragmentBadFragmentGroupIndexIsError pins that a fragment referring to
// a description its sgpd does not hold is rejected.
func TestDefragmentBadFragmentGroupIndexIsError(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	frag, err := mp4.CreateFragment(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	frag.AddFullSample(mp4.FullSample{
		Sample: mp4.Sample{Flags: defragSyncFlags(), Dur: 512, Size: 10}, DecodeTime: 0, Data: defragPayload(1, 10),
	})
	if err := frag.Moof.Traf.AddChild(trafSgpdRoll(-1)); err != nil {
		t.Fatal(err)
	}
	if err := frag.Moof.Traf.AddChild(trafSbgp("roll", 1, 2)); err != nil { // only one entry exists
		t.Fatal(err)
	}
	if err := frag.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	_, err = defragment(t, buf.Bytes(), 1)
	if err == nil || !strings.Contains(err.Error(), "sbgp") {
		t.Errorf("out-of-range fragment group index error %v", err)
	}
}

// TestDefragmentKeepsProgressiveSampleGroups pins that the sample groups of a
// hybrid file's progressive part and of its fragments merge into one list,
// with the samples of both mapping to their own descriptions.
func TestDefragmentKeepsProgressiveSampleGroups(t *testing.T) {
	init := createDefragInit(t)
	stbl := init.Moov.Traks[0].Mdia.Minf.Stbl
	stbl.Stts.SampleCount = []uint32{2}
	stbl.Stts.SampleTimeDelta = []uint32{512}
	stbl.Stsz.SampleNumber = 2
	stbl.Stsz.SampleSize = []uint32{10, 10}
	if err := stbl.Stsc.AddEntry(1, 2, 1); err != nil {
		t.Fatal(err)
	}
	stbl.Stco.ChunkOffset = []uint32{0} // fixed-width placeholder, set below
	stbl.AddChild(&mp4.SgpdBox{
		Version: 1, GroupingType: "roll", DefaultLength: 2,
		SampleGroupEntries: []mp4.SampleGroupEntry{&mp4.RollSampleGroupEntry{RollDistance: -1}},
	})
	stbl.AddChild(&mp4.SbgpBox{
		GroupingType: "roll", SampleCounts: []uint32{2}, GroupDescriptionIndices: []uint32{1},
	})
	var measure bytes.Buffer
	if err := init.Encode(&measure); err != nil {
		t.Fatal(err)
	}
	stbl.Stco.ChunkOffset = []uint32{uint32(measure.Len()) + 8} // after the mdat header
	var hybrid bytes.Buffer
	if err := init.Encode(&hybrid); err != nil {
		t.Fatal(err)
	}
	mdat := &mp4.MdatBox{Data: defragPayload(1, 20)}
	if err := mdat.Encode(&hybrid); err != nil {
		t.Fatal(err)
	}
	// The fragment re-declares nothing; its samples start after the prefix and
	// refer to a description of its own.
	defragGroupSample(t, &hybrid, 1, 1, 1024, 2, 11,
		[]*mp4.SgpdBox{trafSgpdRoll(-2)}, []*mp4.SbgpBox{trafSbgp("roll", 2, 1)})
	out, err := defragment(t, hybrid.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 2 {
		t.Fatalf("got %d sgpd boxes, want the merged progressive and fragment descriptions", len(sgpds))
	}
	if len(sbgps) != 1 {
		t.Fatalf("got %d sbgp boxes, want one merged", len(sbgps))
	}
	if diff := deep.Equal(sbgps[0].SampleCounts, []uint32{2, 2}); diff != nil {
		t.Errorf("sample counts: %v", diff)
	}
	if diff := deep.Equal(sbgps[0].GroupDescriptionIndices, []uint32{1, 2}); diff != nil {
		t.Errorf("group description indices: %v", diff)
	}
}

// TestDefragmentFragmentReferencesTrackLevelDescription pins that a fragment
// that refers to the track-level descriptions with a plain index keeps that
// reference through the merged list.
func TestDefragmentFragmentReferencesTrackLevelDescription(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	stbl := init.Moov.Traks[0].Mdia.Minf.Stbl
	stbl.AddChild(&mp4.SgpdBox{
		Version: 1, GroupingType: "roll", DefaultLength: 2,
		SampleGroupEntries: []mp4.SampleGroupEntry{&mp4.RollSampleGroupEntry{RollDistance: -4}},
	})
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	// A plain index, with no description box in the fragment itself.
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1, nil,
		[]*mp4.SbgpBox{{GroupingType: "roll", SampleCounts: []uint32{2}, GroupDescriptionIndices: []uint32{1}}})
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 1 {
		t.Fatalf("got %d sgpd boxes, want the track-level description", len(sgpds))
	}
	if got := sgpds[0].SampleGroupEntries[0].(*mp4.RollSampleGroupEntry).RollDistance; got != -4 {
		t.Errorf("roll distance %d, want the track-level -4", got)
	}
	if len(sbgps) != 1 || sbgps[0].GroupDescriptionIndices[0] != 1 {
		t.Errorf("sbgp %v, want the description as index 1", sbgps)
	}
}

// TestDefragmentFragmentUsesSgpdDefaultDescription pins that a version 2 sgpd
// in a track fragment gives its default description to the fragment samples
// that no sbgp box maps.
func TestDefragmentFragmentUsesSgpdDefaultDescription(t *testing.T) {
	var buf bytes.Buffer
	init := createDefragInit(t)
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	sgpd := &mp4.SgpdBox{
		Version: 2, GroupingType: "roll", DefaultLength: 2, DefaultGroupDescriptionIndex: 2,
		SampleGroupEntries: []mp4.SampleGroupEntry{
			&mp4.RollSampleGroupEntry{RollDistance: -8},
			&mp4.RollSampleGroupEntry{RollDistance: -6},
		},
	}
	defragGroupSample(t, &buf, 1, 1, 0, 2, 1, []*mp4.SgpdBox{sgpd}, nil)
	out, err := defragment(t, buf.Bytes(), 1)
	if err != nil {
		t.Fatal(err)
	}
	outFile := decodeDefragOutput(t, out.Bytes())
	_, sbgps, sgpds := stblGroups(t, outFile)
	if len(sgpds) != 1 || len(sgpds[0].SampleGroupEntries) != 1 {
		t.Fatalf("got %d sgpd boxes, want only the default entry", len(sgpds))
	}
	if got := sgpds[0].SampleGroupEntries[0].(*mp4.RollSampleGroupEntry).RollDistance; got != -6 {
		t.Errorf("roll distance %d, want the default entry's -6", got)
	}
	if len(sbgps) != 1 || sbgps[0].GroupDescriptionIndices[0] != 1 {
		t.Errorf("sbgp %v, want the default entry as index 1", sbgps)
	}
}
