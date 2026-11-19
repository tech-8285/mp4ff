package mp4

import (
	"fmt"

	"github.com/Eyevinn/mp4ff/bits"
)

// trafGroupIndexOffset is the offset that group_description_index values of a
// sample-to-group box inside a track fragment use to refer to the entries of
// the sample group description box in the same track fragment, the same value
// as sbgpInsideOffset: 0x10001 is the first entry, and lower values refer to
// the track-level descriptions in the stbl (ISO/IEC 14496-12:2026 8.9.4).
const trafGroupIndexOffset = sbgpInsideOffset

// defragSampleDep is the dependency information of one sample: the fields that
// an sdtp box carries and that trun sample flags also signal.
type defragSampleDep struct {
	isLeading     uint8
	dependsOn     uint8
	isDependedOn  uint8
	hasRedundancy uint8
}

// sampleDepFromFlags extracts the dependency information from trun sample
// flags.
func sampleDepFromFlags(flags uint32) defragSampleDep {
	sf := DecodeSampleFlags(flags)
	return defragSampleDep{
		isLeading:     sf.IsLeading,
		dependsOn:     sf.SampleDependsOn,
		isDependedOn:  sf.SampleIsDependedOn,
		hasRedundancy: sf.SampleHasRedundancy,
	}
}

// sampleDepFromSdtp extracts the dependency information from an sdtp entry.
func sampleDepFromSdtp(entry SdtpEntry) defragSampleDep {
	return defragSampleDep{
		isLeading:     entry.IsLeading(),
		dependsOn:     entry.SampleDependsOn(),
		isDependedOn:  entry.SampleIsDependedOn(),
		hasRedundancy: entry.SampleHasRedundancy(),
	}
}

// statesMoreThan reports whether the dependency information says more than
// what the sync status in stss already tells a reader: sample_depends_on 1 for
// a non-sync sample and 2 for a sync one (ISO/IEC 14496-12 8.6.4.2, Table 3),
// and the unknown 0, carry nothing extra.
func (dep defragSampleDep) statesMoreThan(nonSync bool) bool {
	if dep.isLeading != 0 || dep.isDependedOn != 0 || dep.hasRedundancy != 0 {
		return true
	}
	return dep.dependsOn != 0 && dep.dependsOn != impliedDependsOn(nonSync)
}

// impliedDependsOn is the sample_depends_on value that follows from the sync
// status: a sample not listed in stss depends on others, one listed there does
// not.
func impliedDependsOn(nonSync bool) uint8 {
	if nonSync {
		return 1
	}
	return 2
}

// defragGroupKey identifies one sample-to-group mapping: a grouping type and,
// for a version 1 sample-to-group box, its grouping_type_parameter.
type defragGroupKey struct {
	groupingType string
	parameter    uint32
}

// defragGroupDescriptions is the merged sample group description list of one
// grouping type. Entries are deduplicated by their encoding, and a sample
// references one of them by its 1-based index.
type defragGroupDescriptions struct {
	groupingType string
	entries      []SampleGroupEntry
	indexByKey   map[string]uint32 // encoded entry -> 1-based index
}

// register adds entry to the list unless an identical entry is already there,
// and returns its 1-based index in the merged list.
func (g *defragGroupDescriptions) register(entry SampleGroupEntry) (uint32, error) {
	if entry == nil {
		return 0, fmt.Errorf("sample group description of type %q is nil", g.groupingType)
	}
	data, err := encodeSampleGroupEntry(entry)
	if err != nil {
		return 0, fmt.Errorf("sample group description of type %q: %w", g.groupingType, err)
	}
	if g.indexByKey == nil {
		g.indexByKey = make(map[string]uint32)
	}
	key := string(data)
	if idx, ok := g.indexByKey[key]; ok {
		return idx, nil
	}
	g.entries = append(g.entries, entry)
	idx := uint32(len(g.entries))
	g.indexByKey[key] = idx
	return idx, nil
}

// defragGroup is one sample-to-group mapping, with one description index per
// sample of the track.
type defragGroup struct {
	groupingType string
	parameter    uint32
	versionOne   bool // an input box signalled grouping_type_parameter explicitly
	descs        *defragGroupDescriptions
	refs         []uint32 // per sample: 1-based index into descs.entries, 0 = no group
}

// setRef maps the sample at index n to the description index. Missing entries
// are filled with 0, so that refs never carry stale values from before a cut.
func (g *defragGroup) setRef(n int, index uint32) {
	if n >= len(g.refs) {
		g.refs = append(g.refs, make([]uint32, n+1-len(g.refs))...)
	}
	g.refs[n] = index
}

// descriptions returns the merged description list of the grouping type,
// creating it if needed.
func (t *defragTrack) descriptions(groupingType string) *defragGroupDescriptions {
	if t.groupDescsByType == nil {
		t.groupDescsByType = make(map[string]*defragGroupDescriptions)
	}
	if descs, ok := t.groupDescsByType[groupingType]; ok {
		return descs
	}
	descs := &defragGroupDescriptions{groupingType: groupingType}
	t.groupDescsByType[groupingType] = descs
	t.groupDescs = append(t.groupDescs, descs)
	return descs
}

// group returns the sample-to-group mapping for the grouping type and
// grouping_type_parameter, creating it if needed. The mapping is version 1 if
// any input box for it was.
func (t *defragTrack) group(groupingType string, parameter uint32, versionOne bool) *defragGroup {
	if t.groupsByKey == nil {
		t.groupsByKey = make(map[defragGroupKey]*defragGroup)
	}
	key := defragGroupKey{groupingType: groupingType, parameter: parameter}
	if group, ok := t.groupsByKey[key]; ok {
		group.versionOne = group.versionOne || versionOne
		return group
	}
	group := &defragGroup{
		groupingType: groupingType,
		parameter:    parameter,
		versionOne:   versionOne,
		descs:        t.descriptions(groupingType),
	}
	t.groupsByKey[key] = group
	t.groups = append(t.groups, group)
	return group
}

// trimGroups drops the group references of samples from index fromIdx on,
// which were just removed from the track.
func (t *defragTrack) trimGroups(fromIdx int) {
	for _, group := range t.groups {
		if len(group.refs) > fromIdx {
			group.refs = group.refs[:fromIdx]
		}
	}
}

// registerDescriptions merges the entry lists of the given description boxes
// into the track's description lists, and returns the merged index of every
// entry, keyed by grouping type. A grouping type may have only one description
// box in one scope.
func (t *defragTrack) registerDescriptions(sgpds []*SgpdBox) (map[string][]uint32, error) {
	merged := make(map[string][]uint32, len(sgpds))
	for _, sgpd := range sgpds {
		// Version 3 carries a grouping_type_parameter that SgpdBox does not
		// read, so its entries cannot be decoded reliably.
		if sgpd.Version > 2 {
			return nil, fmt.Errorf("sgpd version %d of grouping type %q is not supported",
				sgpd.Version, sgpd.GroupingType)
		}
		if _, exists := merged[sgpd.GroupingType]; exists {
			return nil, fmt.Errorf("more than one sgpd box for grouping type %q", sgpd.GroupingType)
		}
		descs := t.descriptions(sgpd.GroupingType)
		indices := make([]uint32, len(sgpd.SampleGroupEntries))
		for i, entry := range sgpd.SampleGroupEntries {
			idx, err := descs.register(entry)
			if err != nil {
				return nil, err
			}
			indices[i] = idx
		}
		merged[sgpd.GroupingType] = indices
	}
	return merged, nil
}

// hasGroupAt reports whether a sample-to-group box of the grouping type,
// whatever its grouping_type_parameter, maps the sample at index n.
func (t *defragTrack) hasGroupAt(groupingType string, n int) bool {
	for _, group := range t.groups {
		if group.groupingType == groupingType && n < len(group.refs) && group.refs[n] != 0 {
			return true
		}
	}
	return false
}

// collectTrackDescriptions registers the descriptions of the moov sample
// tables, so that fragments can refer to them by their plain index. A
// fragmented file has no progressive samples, but its stbl may still carry
// the descriptions that the fragments use.
func (t *defragTrack) collectTrackDescriptions(stbl *StblBox) error {
	trackLevel, err := t.registerDescriptions(stbl.Sgpds)
	if err != nil {
		return err
	}
	if len(trackLevel) > 0 {
		t.trackGroupIdx = trackLevel
	}
	// A version 2 description box with a default description covers the
	// samples of the track, including those of the fragments.
	for _, sgpd := range stbl.Sgpds {
		if sgpd.Version < 2 || sgpd.DefaultGroupDescriptionIndex == 0 {
			continue
		}
		merged := trackLevel[sgpd.GroupingType]
		if sgpd.DefaultGroupDescriptionIndex > uint32(len(merged)) {
			return fmt.Errorf("sgpd %q has default group description %d, but %d entries",
				sgpd.GroupingType, sgpd.DefaultGroupDescriptionIndex, len(merged))
		}
		t.trackDefaults = append(t.trackDefaults, trackDefault{
			groupingType: sgpd.GroupingType,
			index:        merged[sgpd.DefaultGroupDescriptionIndex-1],
		})
	}
	return nil
}

// trackDefault is the description that a track-level version 2 description box
// gives to the samples of its grouping type that no sample-to-group box maps.
type trackDefault struct {
	groupingType string
	index        uint32 // merged description index
}

// applyTrackDefaults maps the samples in [first, first+nrSamples) that no
// sample-to-group box maps to the track-level default descriptions.
func (t *defragTrack) applyTrackDefaults(first, nrSamples int) {
	for _, def := range t.trackDefaults {
		group := t.group(def.groupingType, 0, false)
		for i := first; i < first+nrSamples; i++ {
			if t.hasGroupAt(def.groupingType, i) {
				continue
			}
			group.setRef(i, def.index)
		}
	}
}

// collectProgressiveGroups maps the progressive samples of the moov sample
// tables to their sample groups and takes their dependencies from an sdtp box.
// There are no progressive samples when the moov holds no sample tables, and
// then there is nothing to map.
func (t *defragTrack) collectProgressiveGroups(stbl *StblBox) error {
	if len(t.samples) == 0 {
		return nil
	}
	seen := make(map[defragGroupKey]bool, len(stbl.Sbgps))
	for _, sbgp := range stbl.Sbgps {
		key := defragGroupKey{groupingType: sbgp.GroupingType, parameter: sbgp.GroupingTypeParameter}
		if seen[key] {
			return fmt.Errorf("more than one sbgp box for grouping type %q with parameter %d",
				sbgp.GroupingType, sbgp.GroupingTypeParameter)
		}
		seen[key] = true
		resolved, err := resolveTrackLevelIndices(sbgp, t.trackGroupIdx[sbgp.GroupingType])
		if err != nil {
			return err
		}
		if err := t.applySbgp(sbgp, resolved, 0, len(t.samples)); err != nil {
			return err
		}
	}
	// The track-level default descriptions apply once every explicit mapping
	// is known, so that a sample keeps the group it was put in above.
	t.applyTrackDefaults(0, len(t.samples))
	if stbl.Sdtp != nil {
		t.sawSdtp = true
		for i, entry := range stbl.Sdtp.Entries {
			if i >= len(t.samples) {
				break
			}
			t.samples[i].dep = sampleDepFromSdtp(entry)
		}
	}
	return nil
}

// collectFragmentGroups registers the descriptions of one track fragment and
// maps the fragment's samples in [first, first+nrSamples) to them. The
// fragment may refer to its own descriptions with the fragment index offset or
// to the track-level descriptions with plain indices.
func (t *defragTrack) collectFragmentGroups(traf *TrafBox, first, nrSamples int) error {
	var sgpds []*SgpdBox
	for _, child := range traf.Children {
		if sgpd, ok := child.(*SgpdBox); ok {
			sgpds = append(sgpds, sgpd)
		}
	}
	locals, err := t.registerDescriptions(sgpds)
	if err != nil {
		return err
	}
	seen := make(map[defragGroupKey]bool)
	for _, child := range traf.Children {
		sbgp, ok := child.(*SbgpBox)
		if !ok {
			continue
		}
		key := defragGroupKey{groupingType: sbgp.GroupingType, parameter: sbgp.GroupingTypeParameter}
		if seen[key] {
			return fmt.Errorf("more than one sbgp box for grouping type %q with parameter %d",
				sbgp.GroupingType, sbgp.GroupingTypeParameter)
		}
		seen[key] = true
		resolved, err := t.resolveFragmentIndices(sbgp, locals[sbgp.GroupingType])
		if err != nil {
			return err
		}
		if err := t.applySbgp(sbgp, resolved, first, nrSamples); err != nil {
			return err
		}
	}
	// A version 2 description box in the track fragment gives the description
	// of the fragment's samples that no sbgp box maps.
	for _, sgpd := range sgpds {
		if err := t.applyFragmentDefaultDescription(sgpd, locals[sgpd.GroupingType], first, nrSamples); err != nil {
			return err
		}
	}
	// Descriptions that the track table declares as defaults cover fragment
	// samples that neither an sbgp nor the fragment's own default maps.
	t.applyTrackDefaults(first, nrSamples)
	return nil
}

// applyFragmentDefaultDescription maps the samples in [first, first+nrSamples)
// that no sample-to-group box maps to the default description of a version 2
// description box in the track fragment.
func (t *defragTrack) applyFragmentDefaultDescription(sgpd *SgpdBox, merged []uint32, first, nrSamples int) error {
	if sgpd.Version < 2 || sgpd.DefaultGroupDescriptionIndex == 0 {
		return nil
	}
	if sgpd.DefaultGroupDescriptionIndex > uint32(len(merged)) {
		return fmt.Errorf("sgpd %q has default group description %d, but %d entries",
			sgpd.GroupingType, sgpd.DefaultGroupDescriptionIndex, len(merged))
	}
	index := merged[sgpd.DefaultGroupDescriptionIndex-1]
	group := t.group(sgpd.GroupingType, 0, false)
	for i := first; i < first+nrSamples; i++ {
		if t.hasGroupAt(sgpd.GroupingType, i) {
			continue
		}
		group.setRef(i, index)
	}
	return nil
}

// resolveTrackLevelIndices maps the group_description_index values of an sbgp
// box in the stbl to merged indices. merged lists the merged index of every
// entry of the track-level description box, or is nil if there is none.
func resolveTrackLevelIndices(sbgp *SbgpBox, merged []uint32) ([]uint32, error) {
	resolved := make([]uint32, len(sbgp.GroupDescriptionIndices))
	for i, idx := range sbgp.GroupDescriptionIndices {
		switch {
		case idx == 0:
		case merged == nil || idx > uint32(len(merged)):
			return nil, fmt.Errorf("sbgp %q refers to group description %d, but the stbl has %d",
				sbgp.GroupingType, idx, len(merged))
		default:
			resolved[i] = merged[idx-1]
		}
	}
	return resolved, nil
}

// resolveFragmentIndices maps the group_description_index values of an sbgp
// box in a track fragment to merged indices. Values above the fragment offset
// refer to the fragment's own description box, lower ones to the track-level
// descriptions in the stbl (ISO/IEC 14496-12:2026 8.9.4).
func (t *defragTrack) resolveFragmentIndices(sbgp *SbgpBox, local []uint32) ([]uint32, error) {
	var trackLevel []uint32
	if t.trackGroupIdx != nil {
		trackLevel = t.trackGroupIdx[sbgp.GroupingType]
	}
	resolved := make([]uint32, len(sbgp.GroupDescriptionIndices))
	for i, idx := range sbgp.GroupDescriptionIndices {
		var out uint32
		switch {
		case idx == 0, idx == trafGroupIndexOffset:
			// Not in a group. The bare fragment offset names no entry, and
			// readers that mask it with the offset reach the same conclusion.
		case idx > trafGroupIndexOffset:
			localNr := idx - trafGroupIndexOffset
			if localNr > uint32(len(local)) {
				return nil, fmt.Errorf("sbgp %q refers to group description %d in the track fragment, "+
					"but its sgpd has %d entries", sbgp.GroupingType, localNr, len(local))
			}
			out = local[localNr-1]
		case idx > uint32(len(trackLevel)):
			return nil, fmt.Errorf("sbgp %q in a track fragment refers to track-level group description %d, "+
				"but the track has %d", sbgp.GroupingType, idx, len(trackLevel))
		default:
			out = trackLevel[idx-1]
		}
		resolved[i] = out
	}
	return resolved, nil
}

// applySbgp maps the samples in [first, first+nrSamples) to the group's
// descriptions according to the sample counts of the sbgp box. More samples
// than the scope holds cannot be mapped and is an error; fewer samples leave
// the rest of the scope without a group.
func (t *defragTrack) applySbgp(sbgp *SbgpBox, resolved []uint32, first, nrSamples int) error {
	if len(sbgp.SampleCounts) != len(resolved) {
		return fmt.Errorf("sbgp %q has %d sample counts but %d description indices",
			sbgp.GroupingType, len(sbgp.SampleCounts), len(resolved))
	}
	var mapped uint64
	for _, count := range sbgp.SampleCounts {
		mapped += uint64(count)
	}
	if mapped > uint64(nrSamples) {
		return fmt.Errorf("sbgp %q maps %d samples, but the scope holds %d",
			sbgp.GroupingType, mapped, nrSamples)
	}
	group := t.group(sbgp.GroupingType, sbgp.GroupingTypeParameter, sbgp.Version == 1)
	sample := first
	for i, count := range sbgp.SampleCounts {
		for j := uint32(0); j < count; j++ {
			group.setRef(sample, resolved[i])
			sample++
		}
	}
	return nil
}

// buildSampleGroups prunes the descriptions that no surviving sample
// references, remaps the references of those that remain, and returns the
// boxes for the track's sample table.
func (t *defragTrack) buildSampleGroups() (sdtp *SdtpBox, sbgps []*SbgpBox, sgpds []*SgpdBox, err error) {
	sdtp = buildSdtp(t.samples, t.sawSdtp)
	referenced := make(map[*defragGroupDescriptions][]bool, len(t.groupDescs))
	for _, group := range t.groups {
		mark, ok := referenced[group.descs]
		if !ok {
			mark = make([]bool, len(group.descs.entries)+1)
			referenced[group.descs] = mark
		}
		for _, ref := range group.refs {
			if ref == 0 {
				continue
			}
			if int(ref) >= len(mark) {
				return nil, nil, nil, fmt.Errorf("internal error: group description index %d is out of range", ref)
			}
			mark[ref] = true
		}
	}
	remaps := make(map[*defragGroupDescriptions][]uint32, len(t.groupDescs))
	for _, descs := range t.groupDescs {
		mark := referenced[descs]
		if mark == nil {
			continue // no mapping ever referred to this description list
		}
		remap := make([]uint32, len(descs.entries)+1)
		kept := make([]SampleGroupEntry, 0, len(descs.entries))
		for i := 1; i <= len(descs.entries); i++ {
			if !mark[i] {
				continue
			}
			kept = append(kept, descs.entries[i-1])
			remap[i] = uint32(len(kept))
		}
		if len(kept) == 0 {
			continue
		}
		sgpd, err := buildSgpdBox(descs.groupingType, kept)
		if err != nil {
			return nil, nil, nil, err
		}
		sgpds = append(sgpds, sgpd)
		remaps[descs] = remap
	}
	for _, group := range t.groups {
		remap, ok := remaps[group.descs]
		if !ok {
			continue // the group's descriptions are all unreferenced
		}
		mapped := make([]uint32, len(group.refs))
		hasGroup := false
		for i, ref := range group.refs {
			if ref == 0 {
				continue
			}
			mapped[i] = remap[ref]
			if mapped[i] != 0 {
				hasGroup = true
			}
		}
		if !hasGroup {
			continue
		}
		if sbgp := buildSbgpBox(group.groupingType, group.parameter, group.versionOne, mapped); sbgp != nil {
			sbgps = append(sbgps, sbgp)
		}
	}
	return sdtp, sbgps, sgpds, nil
}

// encodeSampleGroupEntry encodes one sample group description entry and
// returns its bytes, which are what a description_length counts.
func encodeSampleGroupEntry(entry SampleGroupEntry) ([]byte, error) {
	size := entry.Size()
	if uint64(int(size)) != size {
		return nil, fmt.Errorf("size %d of sample group description does not fit memory", size)
	}
	sw := bits.NewFixedSliceWriter(int(size))
	entry.Encode(sw)
	if err := sw.AccError(); err != nil {
		return nil, fmt.Errorf("sample group description of type %q does not fit its reported size %d: %w",
			entry.Type(), size, err)
	}
	return sw.Bytes(), nil
}

// buildSdtp returns an sdtp box with one entry per sample when the input
// carried dependency information, and nil otherwise so that input without it
// keeps the same output as before. An sdtp box in the input counts as such
// information even when its entries are all unknown.
func buildSdtp(samples []defragSample, sawSdtp bool) *SdtpBox {
	anyInfo := sawSdtp
	entries := make([]SdtpEntry, len(samples))
	for i, sample := range samples {
		dep := sample.dep
		entries[i] = NewSdtpEntry(dep.isLeading, dep.dependsOn, dep.isDependedOn, dep.hasRedundancy)
		if dep.statesMoreThan(sample.nonSync) {
			anyInfo = true
		}
	}
	if !anyInfo {
		return nil
	}
	return &SdtpBox{Entries: entries}
}

// buildSgpdBox builds the description box of one grouping type. A common
// entry length is signalled by default_length, and individual lengths
// otherwise.
func buildSgpdBox(groupingType string, entries []SampleGroupEntry) (*SgpdBox, error) {
	lengths := make([]uint32, len(entries))
	uniform := true
	for i, entry := range entries {
		data, err := encodeSampleGroupEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("sample group description of type %q: %w", groupingType, err)
		}
		lengths[i] = uint32(len(data))
		if i > 0 && lengths[i] != lengths[0] {
			uniform = false
		}
	}
	sgpd := &SgpdBox{Version: 1, GroupingType: groupingType, SampleGroupEntries: entries}
	if uniform && lengths[0] != 0 {
		sgpd.DefaultLength = lengths[0]
	} else {
		sgpd.DescriptionLengths = lengths
	}
	return sgpd, nil
}

// buildSbgpBox run-length encodes one sample-to-group mapping. Runs of
// ungrouped samples at the end are dropped, so an sbgp box without groups is
// nil. The box is version 1 when the input signalled grouping_type_parameter
// or gave a nonzero one.
func buildSbgpBox(groupingType string, parameter uint32, versionOne bool, refs []uint32) *SbgpBox {
	if len(refs) == 0 {
		return nil
	}
	b := &SbgpBox{GroupingType: groupingType}
	if versionOne || parameter != 0 {
		b.Version = 1
		b.GroupingTypeParameter = parameter
	}
	for _, ref := range refs {
		if n := len(b.GroupDescriptionIndices); n > 0 && b.GroupDescriptionIndices[n-1] == ref {
			b.SampleCounts[n-1]++
			continue
		}
		b.SampleCounts = append(b.SampleCounts, 1)
		b.GroupDescriptionIndices = append(b.GroupDescriptionIndices, ref)
	}
	for n := len(b.GroupDescriptionIndices); n > 0 && b.GroupDescriptionIndices[n-1] == 0; n-- {
		b.SampleCounts = b.SampleCounts[:n-1]
		b.GroupDescriptionIndices = b.GroupDescriptionIndices[:n-1]
	}
	if len(b.GroupDescriptionIndices) == 0 {
		return nil
	}
	return b
}
