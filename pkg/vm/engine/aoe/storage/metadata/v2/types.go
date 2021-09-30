// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and

package metadata

import (
	"errors"
	"fmt"
	"matrixone/pkg/container/types"
	"matrixone/pkg/vm/engine/aoe/storage/common"
	"sync/atomic"
)

const (
	MinUncommitId = ^uint64(0) / 2
)

var uncommitId = MinUncommitId

func nextUncommitId() uint64 {
	return atomic.AddUint64(&uncommitId, uint64(1)) - 1
}

func IsTransientCommitId(id uint64) bool {
	return id >= MinUncommitId
}

type State = uint8

const (
	STInited State = iota
	STFull
	STClosed
	STSorted
)

type OpT uint8

const (
	OpReserved OpT = iota
	OpCreate
	OpUpgradeFull
	OpUpgradeClose
	OpUpgradeSorted
	OpSoftDelete
	OpHardDelete
)

var OpNames = map[OpT]string{
	OpCreate:        "Create",
	OpUpgradeFull:   "UpgradeFull",
	OpUpgradeClose:  "UpgradeClose",
	OpUpgradeSorted: "UpgradeSorted",
	OpSoftDelete:    "SoftDelete",
	OpHardDelete:    "HardDelete",
}

func OpName(op OpT) string {
	return OpNames[op]
}

type CommitInfo struct {
	common.SSLLNode `json:"-"`
	CommitId        uint64
	TranId          uint64
	Op              OpT
	ExternalIndex   *ExternalIndex
	PrevIndex       *ExternalIndex
	AppliedIndex    *ExternalIndex
}

func (info *CommitInfo) IsHardDeleted() bool {
	return info.Op == OpHardDelete
}

func (info *CommitInfo) IsSoftDeleted() bool {
	return info.Op == OpSoftDelete
}

func (info *CommitInfo) PString(level PPLevel) string {
	s := fmt.Sprintf("CInfo: ")
	var curr, prev common.ISSLLNode
	curr = info
	for curr != nil {
		if prev != nil {
			s = fmt.Sprintf("%s -> ", s)
		}
		cInfo := curr.(*CommitInfo)
		s = fmt.Sprintf("%s(%s,%d)", s, OpName(cInfo.Op), cInfo.CommitId)
		// s = fmt.Sprintf("%s(%s,%d,%d)", s, OpName(info.Op), info.TranId-MinUncommitId, info.CommitId)
		prev = curr
		curr = curr.GetNext()
	}
	return s
}

// TODO: remove it. Not be used later
func (info *CommitInfo) GetAppliedIndex() (uint64, bool) {
	if info.AppliedIndex != nil {
		return info.AppliedIndex.Id, true
	}
	if info.ExternalIndex != nil && info.ExternalIndex.IsApplied() {
		return info.ExternalIndex.Id, true
	}

	if info.PrevIndex != nil {
		return info.PrevIndex.Id, true
	}
	return 0, false
}

// SetIndex changes the current index to previous index if exists, and
// sets the current index to idx.
func (info *CommitInfo) SetIndex(idx LogIndex) error {
	if info.ExternalIndex != nil {
		if !info.ExternalIndex.IsApplied() {
			return errors.New(fmt.Sprintf("already has applied index: %d", info.ExternalIndex.Id))
		}
		info.PrevIndex = info.ExternalIndex
		info.ExternalIndex = &idx
	} else {
		if info.PrevIndex != nil {
			return errors.New(fmt.Sprintf("no index but has prev index: %d", info.PrevIndex.Id))
		}
		info.ExternalIndex = &idx
	}
	return nil
}

type Sequence struct {
	nextTableId   uint64
	nextSegmentId uint64
	nextBlockId   uint64
	nextCommitId  uint64
	nextIndexId   uint64
}

func (s *Sequence) NextTableId() uint64 {
	return atomic.AddUint64(&s.nextTableId, uint64(1))
}

func (s *Sequence) NextSegmentId() uint64 {
	return atomic.AddUint64(&s.nextSegmentId, uint64(1))
}

func (s *Sequence) NextBlockId() uint64 {
	return atomic.AddUint64(&s.nextBlockId, uint64(1))
}

func (s *Sequence) NextCommitId() uint64 {
	return atomic.AddUint64(&s.nextCommitId, uint64(1))
}

func (s *Sequence) NextIndexId() uint64 {
	return atomic.AddUint64(&s.nextIndexId, uint64(1))
}

func (s *Sequence) NextUncommitId() uint64 {
	return nextUncommitId()
}

func EstimateColumnBlockSize(colIdx int, meta *Block) uint64 {
	switch meta.Segment.Table.Schema.ColDefs[colIdx].Type.Oid {
	case types.T_json, types.T_char, types.T_varchar:
		return meta.Segment.Table.Schema.BlockMaxRows * 2 * 4
	default:
		return meta.Segment.Table.Schema.BlockMaxRows * uint64(meta.Segment.Table.Schema.ColDefs[colIdx].Type.Size)
	}
}

func EstimateBlockSize(meta *Block) uint64 {
	size := uint64(0)
	for colIdx, _ := range meta.Segment.Table.Schema.ColDefs {
		size += EstimateColumnBlockSize(colIdx, meta)
	}
	return size
}
