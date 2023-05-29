// Copyright 2022 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package indexwrapper

import (
	"context"

	"github.com/RoaringBitmap/roaring"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/fileservice"
	"github.com/matrixorigin/matrixone/pkg/objectio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/blockio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/index"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/model"
)

type ImmutIndex struct {
	zm       index.ZM
	bf       objectio.BloomFilter
	location objectio.Location
	cache    model.LRUCache
	fs       fileservice.FileService
}

func NewImmutIndex(
	zm index.ZM,
	bf objectio.BloomFilter,
	location objectio.Location,
	cache model.LRUCache,
	fs fileservice.FileService,
) ImmutIndex {
	return ImmutIndex{
		zm:       zm,
		bf:       bf,
		location: location,
		cache:    cache,
		fs:       fs,
	}
}

func (idx ImmutIndex) BatchDedup(
	ctx context.Context,
	keys containers.Vector,
	keysZM index.ZM,
) (sels *roaring.Bitmap, err error) {
	var exist bool
	if keysZM.Valid() {
		if exist = idx.zm.FastIntersect(keysZM); !exist {
			// all keys are not in [min, max]. definitely not
			return
		}
	} else {
		if exist = idx.zm.FastContainsAny(keys); !exist {
			// all keys are not in [min, max]. definitely not
			return
		}
	}

	// some keys are in [min, max]. check bloomfilter for those keys

	var buf []byte
	if len(idx.bf) > 0 {
		buf = idx.bf.GetBloomFilter(uint32(idx.location.ID()))
	} else {
		var bf objectio.BloomFilter
		if bf, err = blockio.LoadBF(
			ctx,
			idx.location,
			idx.cache,
			idx.fs,
			false,
		); err != nil {
			return
		}
		buf = bf.GetBloomFilter(uint32(idx.location.ID()))
	}

	bfIndex := index.NewEmptyBinaryFuseFilter()
	if err = index.DecodeBloomFilter(bfIndex, buf); err != nil {
		return
	}

	if exist, sels, err = bfIndex.MayContainsAnyKeys(keys); err != nil {
		// check bloomfilter has some unknown error. return err
		err = TranslateError(err)
		return
	} else if !exist {
		// all keys were checked. definitely not
		return
	}

	err = moerr.GetOkExpectedPossibleDup()
	return
}

func (idx ImmutIndex) Dedup(ctx context.Context, key any) (err error) {
	exist := idx.zm.Contains(key)
	// 1. if not in [min, max], key is definitely not found
	if !exist {
		return
	}
	var buf []byte
	if len(idx.bf) > 0 {
		buf = idx.bf.GetBloomFilter(uint32(idx.location.ID()))
	} else {
		var bf objectio.BloomFilter
		if bf, err = blockio.LoadBF(
			ctx,
			idx.location,
			idx.cache,
			idx.fs,
			false,
		); err != nil {
			return
		}
		buf = bf.GetBloomFilter(uint32(idx.location.ID()))
	}

	bfIndex := index.NewEmptyBinaryFuseFilter()
	if err = index.DecodeBloomFilter(bfIndex, buf); err != nil {
		return
	}

	v := types.EncodeValue(key, idx.zm.GetType())
	exist, err = bfIndex.MayContainsKey(v)
	// 2. check bloomfilter has some error. return err
	if err != nil {
		err = TranslateError(err)
		return
	}
	// 3. all keys were checked. definitely not
	if !exist {
		return
	}
	err = moerr.GetOkExpectedPossibleDup()
	return
}