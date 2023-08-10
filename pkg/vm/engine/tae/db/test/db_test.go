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
// limitations under the License.

package test

import (
	"bytes"
	"context"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sort"

	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/objectio"

	"github.com/matrixorigin/matrixone/pkg/util/fault"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/blockio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/db"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/db/gc"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/db/testutil"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tables"

	"github.com/matrixorigin/matrixone/pkg/pb/api"
	"github.com/matrixorigin/matrixone/pkg/pb/timestamp"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/db/checkpoint"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/logtail"

	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/common/mpool"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/nulls"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/testutils/config"

	pkgcatalog "github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/handle"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/txnif"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/options"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tables/jobs"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tables/txnentries"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tasks"
	ops "github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tasks/worker"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/testutils"
	"github.com/panjf2000/ants/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ModuleName = "TAEDB"
)

func TestAppend(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.NewTestEngine(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(14, 3)
	schema.BlockMaxRows = options.DefaultBlockMaxRows
	schema.SegmentMaxBlocks = options.DefaultBlocksPerSegment
	tae.BindSchema(schema)
	data := catalog.MockBatch(schema, int(schema.BlockMaxRows*2))
	defer data.Close()
	bats := data.Split(4)
	now := time.Now()
	tae.CreateRelAndAppend(bats[0], true)
	t.Log(time.Since(now))
	tae.CheckRowsByScan(bats[0].Length(), false)

	txn, rel := tae.GetRelation()
	err := rel.Append(context.Background(), bats[1])
	assert.NoError(t, err)
	// FIXME
	// testutil.CheckAllColRowsByScan(t, rel, bats[0].Length()+bats[1].Length(), false)
	err = rel.Append(context.Background(), bats[2])
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
	tae.CheckRowsByScan(bats[0].Length()+bats[1].Length()+bats[2].Length(), false)
	t.Log(tae.Catalog.SimplePPString(common.PPL1))
}

func TestAppend2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()

	// this task won't affect logic of TestAppend2, it just prints logs about dirty count
	// forest := logtail.NewDirtyCollector(db.LogtailMgr, opts.Clock, db.Catalog, new(catalog.LoopProcessor))
	// hb := ops.NewHeartBeaterWithFunc(5*time.Millisecond, func() {
	// 	forest.Run()
	// 	t.Log(forest.String())
	// }, nil)
	// hb.Start()
	// defer hb.Stop()

	schema := catalog.MockSchemaAll(13, 3)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 10
	testutil.CreateRelation(t, db, "db", schema, true)

	totalRows := uint64(schema.BlockMaxRows * 30)
	bat := catalog.MockBatch(schema, int(totalRows))
	defer bat.Close()
	bats := bat.Split(100)

	var wg sync.WaitGroup
	pool, _ := ants.NewPool(80)
	defer pool.Release()

	start := time.Now()
	for _, data := range bats {
		wg.Add(1)
		err := pool.Submit(testutil.AppendClosure(t, data, schema.Name, db, &wg))
		assert.Nil(t, err)
	}
	wg.Wait()
	t.Logf("Append %d rows takes: %s", totalRows, time.Since(start))
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		testutil.CheckAllColRowsByScan(t, rel, int(totalRows), false)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	t.Log(db.Catalog.SimplePPString(common.PPL1))

	now := time.Now()
	testutils.WaitExpect(20000, func() bool {
		return db.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	t.Log(time.Since(now))
	t.Logf("Checkpointed: %d", db.Runtime.Scheduler.GetCheckpointedLSN())
	t.Logf("GetPenddingLSNCnt: %d", db.Runtime.Scheduler.GetPenddingLSNCnt())
	assert.Equal(t, uint64(0), db.Runtime.Scheduler.GetPenddingLSNCnt())
	t.Log(db.Catalog.SimplePPString(common.PPL1))
	wg.Add(1)
	testutil.AppendFailClosure(t, bats[0], schema.Name, db, &wg)()
	wg.Wait()
}

func TestAppend3(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchema(2, 0)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	testutil.CreateRelation(t, tae, "db", schema, true)
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	testutil.AppendClosure(t, bat, schema.Name, tae, &wg)()
	wg.Wait()
	testutils.WaitExpect(2000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	// t.Log(tae.Catalog.SimplePPString(common.PPL1))
	wg.Add(1)
	testutil.AppendFailClosure(t, bat, schema.Name, tae, &wg)()
	wg.Wait()
}

func TestAppend4(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema1 := catalog.MockSchemaAll(18, 14)
	schema2 := catalog.MockSchemaAll(18, 15)
	schema3 := catalog.MockSchemaAll(18, 16)
	schema4 := catalog.MockSchemaAll(18, 11)
	schema1.BlockMaxRows = 10
	schema2.BlockMaxRows = 10
	schema3.BlockMaxRows = 10
	schema4.BlockMaxRows = 10
	schema1.SegmentMaxBlocks = 2
	schema2.SegmentMaxBlocks = 2
	schema3.SegmentMaxBlocks = 2
	schema4.SegmentMaxBlocks = 2
	schemas := []*catalog.Schema{schema1, schema2, schema3, schema4}
	testutil.CreateDB(t, tae, testutil.DefaultTestDB)
	for _, schema := range schemas {
		bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*3-1))
		defer bat.Close()
		bats := bat.Split(1)
		testutil.CreateRelation(t, tae, testutil.DefaultTestDB, schema, false)
		for i := range bats {
			txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
			err := rel.Append(context.Background(), bats[i])
			assert.NoError(t, err)
			err = txn.Commit(context.Background())
			assert.NoError(t, err)
		}
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		testutil.CheckAllColRowsByScan(t, rel, bat.Length(), false)

		v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
		filter := handle.NewEQFilter(v)
		err := rel.DeleteByFilter(context.Background(), filter)
		assert.NoError(t, err)
		err = txn.Commit(context.Background())
		assert.NoError(t, err)

		txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
		testutil.CheckAllColRowsByScan(t, rel, bat.Length()-1, true)
		err = txn.Commit(context.Background())
		assert.NoError(t, err)
		testutil.CompactBlocks(t, 0, tae, testutil.DefaultTestDB, schema, false)
		txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
		testutil.CheckAllColRowsByScan(t, rel, bat.Length()-1, false)
		err = txn.Commit(context.Background())
		assert.NoError(t, err)
	}
}

func testCRUD(t *testing.T, tae *db.DB, schema *catalog.Schema) {
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*(uint32(schema.SegmentMaxBlocks)+1)-1))
	defer bat.Close()
	bats := bat.Split(4)

	var updateColIdx int
	if schema.GetSingleSortKeyIdx() >= 17 {
		updateColIdx = 0
	} else {
		updateColIdx = schema.GetSingleSortKeyIdx() + 1
	}

	testutil.CreateRelationAndAppend(t, 0, tae, testutil.DefaultTestDB, schema, bats[0], false)

	txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
	err := rel.Append(context.Background(), bats[0])
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrDuplicateEntry))
	testutil.CheckAllColRowsByScan(t, rel, bats[0].Length(), false)
	v := bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(2)
	filter := handle.NewEQFilter(v)
	err = rel.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)

	oldv := bats[0].Vecs[updateColIdx].Get(5)
	oldvIsNull := bats[0].Vecs[updateColIdx].IsNull(5)

	v = bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(5)
	ufilter := handle.NewEQFilter(v)
	{
		ot := reflect.ValueOf(&oldv).Elem()
		nv := reflect.ValueOf(int8(99))
		if nv.CanConvert(reflect.TypeOf(oldv)) {
			ot.Set(nv.Convert(reflect.TypeOf(oldv)))
		}
	}
	err = rel.UpdateByFilter(context.Background(), ufilter, uint16(updateColIdx), oldv, oldvIsNull)
	assert.NoError(t, err)

	testutil.CheckAllColRowsByScan(t, rel, bats[0].Length()-1, true)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
	testutil.CheckAllColRowsByScan(t, rel, bats[0].Length()-1, true)
	for _, b := range bats[1:] {
		err = rel.Append(context.Background(), b)
		assert.NoError(t, err)
	}
	testutil.CheckAllColRowsByScan(t, rel, bat.Length()-1, true)
	assert.NoError(t, txn.Commit(context.Background()))

	testutil.CompactBlocks(t, 0, tae, testutil.DefaultTestDB, schema, false)

	txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
	testutil.CheckAllColRowsByScan(t, rel, bat.Length()-1, false)
	v = bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter = handle.NewEQFilter(v)
	err = rel.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, bat.Length()-2, true)
	assert.NoError(t, txn.Commit(context.Background()))

	// After merging blocks, the logic of read data is modified
	//compactSegs(t, tae, schema)

	txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
	//testutil.CheckAllColRowsByScan(t, rel, bat.Length()-2, false)
	testutil.CheckAllColRowsByScan(t, rel, bat.Length()-1, false)
	assert.NoError(t, txn.Commit(context.Background()))

	// t.Log(rel.GetMeta().(*catalog.TableEntry).PPString(common.PPL1, 0, ""))
	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	db, err := txn.GetDatabase(testutil.DefaultTestDB)
	assert.NoError(t, err)
	_, err = db.DropRelationByName(schema.Name)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestCRUD(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer tae.Close()
	testutil.CreateDB(t, tae, testutil.DefaultTestDB)
	testutil.WithTestAllPKType(t, tae, testCRUD)
}

func TestTableHandle(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	db := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer db.Close()

	schema := catalog.MockSchema(2, 0)
	schema.BlockMaxRows = 1000
	schema.SegmentMaxBlocks = 2

	txn, _ := db.StartTxn(nil)
	database, _ := txn.CreateDatabase("db", "", "")
	rel, _ := database.CreateRelation(schema)

	tableMeta := rel.GetMeta().(*catalog.TableEntry)
	t.Log(tableMeta.String())
	table := tableMeta.GetTableData()

	handle := table.GetHandle()
	appender, err := handle.GetAppender()
	assert.Nil(t, appender)
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrAppendableSegmentNotFound))
}

func TestCreateBlock(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	db := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer db.Close()

	txn, _ := db.StartTxn(nil)
	database, _ := txn.CreateDatabase("db", "", "")
	schema := catalog.MockSchemaAll(13, 12)
	rel, err := database.CreateRelation(schema)
	assert.Nil(t, err)
	seg, err := rel.CreateSegment(false)
	assert.Nil(t, err)
	blk1, err := seg.CreateBlock(false)
	assert.Nil(t, err)
	blk2, err := seg.CreateNonAppendableBlock(nil)
	assert.Nil(t, err)
	lastAppendable := seg.GetMeta().(*catalog.SegmentEntry).LastAppendableBlock()
	assert.Equal(t, blk1.Fingerprint().BlockID, lastAppendable.ID)
	assert.True(t, lastAppendable.IsAppendable())
	blk2Meta := blk2.GetMeta().(*catalog.BlockEntry)
	assert.False(t, blk2Meta.IsAppendable())

	t.Log(db.Catalog.SimplePPString(common.PPL1))
	assert.Nil(t, txn.Commit(context.Background()))
	t.Log(db.Catalog.SimplePPString(common.PPL1))
}

func TestNonAppendableBlock(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	db := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer db.Close()
	schema := catalog.MockSchemaAll(13, 1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2

	bat := catalog.MockBatch(schema, 8)
	defer bat.Close()

	testutil.CreateRelation(t, db, "db", schema, true)

	{
		txn, _ := db.StartTxn(nil)
		database, err := txn.GetDatabase("db")
		assert.Nil(t, err)
		rel, err := database.GetRelationByName(schema.Name)
		readSchema := rel.Schema()
		assert.Nil(t, err)
		seg, err := rel.CreateSegment(false)
		assert.Nil(t, err)
		blk, err := seg.CreateNonAppendableBlock(nil)
		assert.Nil(t, err)
		dataBlk := blk.GetMeta().(*catalog.BlockEntry).GetBlockData()
		sid := objectio.NewSegmentid()
		name := objectio.BuildObjectName(sid, 0)
		writer, err := blockio.NewBlockWriterNew(dataBlk.GetFs().Service, name, 0, nil)
		assert.Nil(t, err)
		_, err = writer.WriteBatch(containers.ToCNBatch(bat))
		assert.Nil(t, err)
		blocks, _, err := writer.Sync(context.Background())
		assert.Nil(t, err)
		metaLoc := blockio.EncodeLocation(
			writer.GetName(),
			blocks[0].GetExtent(),
			uint32(bat.Length()),
			blocks[0].GetID())
		blk.UpdateMetaLoc(metaLoc)
		v, _, err := dataBlk.GetValue(context.Background(), txn, readSchema, 4, 2)
		assert.Nil(t, err)
		expectVal := bat.Vecs[2].Get(4)
		assert.Equal(t, expectVal, v)
		assert.Equal(t, bat.Vecs[0].Length(), blk.Rows())

		view, err := dataBlk.GetColumnDataById(context.Background(), txn, readSchema, 2)
		assert.Nil(t, err)
		defer view.Close()
		assert.Nil(t, view.DeleteMask)
		assert.Equal(t, bat.Vecs[2].Length(), view.Length())

		_, err = dataBlk.RangeDelete(txn, 1, 2, nil, handle.DT_Normal)
		assert.Nil(t, err)

		view, err = dataBlk.GetColumnDataById(context.Background(), txn, readSchema, 2)
		assert.Nil(t, err)
		defer view.Close()
		assert.True(t, view.DeleteMask.Contains(1))
		assert.True(t, view.DeleteMask.Contains(2))
		assert.Equal(t, bat.Vecs[2].Length(), view.Length())

		// _, err = dataBlk.Update(txn, 3, 2, int32(999))
		// assert.Nil(t, err)

		view, err = dataBlk.GetColumnDataById(context.Background(), txn, readSchema, 2)
		assert.Nil(t, err)
		defer view.Close()
		assert.True(t, view.DeleteMask.Contains(1))
		assert.True(t, view.DeleteMask.Contains(2))
		assert.Equal(t, bat.Vecs[2].Length(), view.Length())
		// v = view.GetData().Get(3)
		// assert.Equal(t, int32(999), v)

		assert.Nil(t, txn.Commit(context.Background()))
	}
}

func TestCreateSegment(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(1, 0)
	txn, _ := tae.StartTxn(nil)
	db, err := txn.CreateDatabase("db", "", "")
	assert.Nil(t, err)
	rel, err := db.CreateRelation(schema)
	assert.Nil(t, err)
	_, err = rel.CreateNonAppendableSegment(false)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	bat := catalog.MockBatch(schema, 5)
	defer bat.Close()

	testutil.AppendClosure(t, bat, schema.Name, tae, nil)()

	segCnt := 0
	processor := new(catalog.LoopProcessor)
	processor.SegmentFn = func(segment *catalog.SegmentEntry) error {
		segCnt++
		return nil
	}
	err = tae.Catalog.RecurLoop(processor)
	assert.Nil(t, err)
	assert.Equal(t, 2+3, segCnt)
	t.Log(tae.Catalog.SimplePPString(common.PPL1))
}

func TestCompactBlock1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()
	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 4
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	testutil.CreateRelationAndAppend(t, 0, db, "db", schema, bat, true)
	t.Log(db.Catalog.SimplePPString(common.PPL1))

	v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(2)
	filter := handle.NewEQFilter(v)
	// 1. No updates and deletes
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		blkMeta := testutil.GetOneBlockMeta(rel)
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta, db.Runtime)
		assert.Nil(t, err)
		preparer, _, err := task.PrepareData(context.Background())
		assert.Nil(t, err)
		assert.NotNil(t, preparer.Columns)
		defer preparer.Close()
		for col := 0; col < len(bat.Vecs); col++ {
			for row := 0; row < bat.Vecs[0].Length(); row++ {
				exp := bat.Vecs[col].Get(row)
				act := preparer.Columns.Vecs[col].Get(row)
				assert.Equal(t, exp, act)
			}
		}
		err = rel.DeleteByFilter(context.Background(), filter)
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, _ := db.StartTxn(nil)
		database, err := txn.GetDatabase("db")
		assert.Nil(t, err)
		rel, err := database.GetRelationByName(schema.Name)
		assert.Nil(t, err)
		v = bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
		filter.Val = v
		id, _, err := rel.GetByFilter(context.Background(), filter)
		assert.Nil(t, err)
		seg, _ := rel.GetSegment(id.SegmentID())
		block, err := seg.GetBlock(id.BlockID)
		assert.Nil(t, err)
		blkMeta := block.GetMeta().(*catalog.BlockEntry)
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta, db.Runtime)
		assert.Nil(t, err)
		preparer, _, err := task.PrepareData(context.Background())
		assert.Nil(t, err)
		defer preparer.Close()
		assert.Equal(t, bat.Vecs[0].Length()-1, preparer.Columns.Vecs[0].Length())
		{
			txn, _ := db.StartTxn(nil)
			database, err := txn.GetDatabase("db")
			assert.Nil(t, err)
			rel, err := database.GetRelationByName(schema.Name)
			assert.Nil(t, err)
			v = bat.Vecs[schema.GetSingleSortKeyIdx()].Get(4)
			filter.Val = v
			id, offset, err := rel.GetByFilter(context.Background(), filter)
			assert.Nil(t, err)
			err = rel.RangeDelete(id, offset, offset, handle.DT_Normal)
			assert.Nil(t, err)
			f2 := handle.NewEQFilter(v.(int32) + 1)
			err = rel.UpdateByFilter(context.Background(), f2, 3, int64(99), false)
			assert.Nil(t, err)
			assert.Nil(t, txn.Commit(context.Background()))
		}
		task, err = jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta, db.Runtime)
		assert.Nil(t, err)
		preparer, _, err = task.PrepareData(context.Background())
		assert.Nil(t, err)
		defer preparer.Close()
		assert.Equal(t, bat.Vecs[0].Length()-1, preparer.Columns.Vecs[0].Length())
		var maxTs types.TS
		{
			txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
			seg, err := rel.GetSegment(id.SegmentID())
			assert.Nil(t, err)
			blk, err := seg.GetBlock(id.BlockID)
			assert.Nil(t, err)
			blkMeta := blk.GetMeta().(*catalog.BlockEntry)
			task, err = jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta, db.Runtime)
			assert.Nil(t, err)
			preparer, _, err := task.PrepareData(context.Background())
			assert.Nil(t, err)
			defer preparer.Close()
			assert.Equal(t, bat.Vecs[0].Length()-3, preparer.Columns.Vecs[0].Length())
			maxTs = txn.GetStartTS()
		}

		dataBlock := block.GetMeta().(*catalog.BlockEntry).GetBlockData()
		changes, err := dataBlock.CollectChangesInRange(context.Background(), txn.GetStartTS(), maxTs.Next())
		assert.NoError(t, err)
		assert.Equal(t, 2, changes.DeleteMask.GetCardinality())

		destBlock, err := seg.CreateNonAppendableBlock(nil)
		assert.Nil(t, err)
		m := destBlock.GetMeta().(*catalog.BlockEntry)
		txnEntry := txnentries.NewCompactBlockEntry(
			txn, block, destBlock, nil, nil, db.Runtime,
		)
		err = txn.LogTxnEntry(m.GetSegment().GetTable().GetDB().ID, destBlock.Fingerprint().TableID, txnEntry, []*common.ID{block.Fingerprint()})
		assert.Nil(t, err)
		assert.Nil(t, err)
		err = txn.Commit(context.Background())
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnWWConflict))
	}
}

func TestAddBlksWithMetaLoc(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()

	worker := ops.NewOpWorker(context.Background(), "xx")
	worker.Start()
	defer worker.Stop()
	schema := catalog.MockSchemaAll(13, 2)
	schema.Name = "tb-0"
	schema.BlockMaxRows = 20
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*4))
	defer bat.Close()
	bats := bat.Split(4)
	{
		txn, _, rel := testutil.CreateRelationNoCommit(t, db, "db", schema, true)
		err := rel.Append(context.Background(), bats[0])
		assert.NoError(t, err)
		err = rel.Append(context.Background(), bats[1])
		assert.NoError(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}
	//compact blocks
	var newBlockFp1 *common.ID
	var metaLoc1 objectio.Location
	var newBlockFp2 *common.ID
	var metaLoc2 objectio.Location
	{
		txn, rel := testutil.GetRelation(t, 0, db, "db", schema.Name)
		it := rel.MakeBlockIt()
		blkMeta1 := it.GetBlock().GetMeta().(*catalog.BlockEntry)
		it.Next()
		blkMeta2 := it.GetBlock().GetMeta().(*catalog.BlockEntry)
		task1, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta1, db.Runtime)
		assert.NoError(t, err)
		task2, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta2, db.Runtime)
		assert.NoError(t, err)
		worker.SendOp(task1)
		worker.SendOp(task2)
		err = task1.WaitDone()
		assert.NoError(t, err)
		err = task2.WaitDone()
		assert.NoError(t, err)
		newBlockFp1 = task1.GetNewBlock().Fingerprint()
		metaLoc1 = task1.GetNewBlock().GetMetaLoc()
		newBlockFp2 = task2.GetNewBlock().Fingerprint()
		metaLoc2 = task2.GetNewBlock().GetMetaLoc()
		assert.Nil(t, txn.Commit(context.Background()))
	}
	//read new non-appendable block data and check
	{
		txn, rel := testutil.GetRelation(t, 0, db, "db", schema.Name)
		assert.True(t, newBlockFp2.SegmentID().Eq(*newBlockFp1.SegmentID()))
		seg, err := rel.GetSegment(newBlockFp1.SegmentID())
		assert.Nil(t, err)
		blk1, err := seg.GetBlock(newBlockFp1.BlockID)
		assert.Nil(t, err)
		blk2, err := seg.GetBlock(newBlockFp2.BlockID)
		assert.Nil(t, err)

		view1, err := blk1.GetColumnDataById(context.Background(), 2)
		assert.NoError(t, err)
		defer view1.Close()
		assert.True(t, view1.GetData().Equals(bats[0].Vecs[2]))
		assert.Equal(t, blk1.Rows(), bats[0].Vecs[2].Length())

		view2, err := blk2.GetColumnDataById(context.Background(), 2)
		assert.NoError(t, err)
		defer view2.Close()
		assert.True(t, view2.GetData().Equals(bats[1].Vecs[2]))
		assert.Equal(t, blk2.Rows(), bats[1].Vecs[2].Length())
		assert.Nil(t, txn.Commit(context.Background()))
	}

	{
		schema.Name = "tb-1"
		txn, _, rel := testutil.CreateRelationNoCommit(t, db, "db", schema, false)
		txn.SetDedupType(txnif.FullSkipWorkSpaceDedup)
		err := rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{metaLoc1})
		assert.Nil(t, err)
		err = rel.Append(context.Background(), bats[0])
		assert.Nil(t, err)

		err = rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{metaLoc2})
		assert.Nil(t, err)
		err = rel.Append(context.Background(), bats[1])
		assert.Nil(t, err)
		//err = rel.RangeDeleteLocal(start, end)
		//assert.Nil(t, err)
		//assert.True(t, rel.IsLocalDeleted(start, end))
		err = txn.Commit(context.Background())
		assert.Nil(t, err)

		//"tb-1" table now has one committed non-appendable segment which contains
		//two non-appendable block, and one committed appendable segment which contains two appendable block.

		//do deduplication check against sanpshot data.
		txn, rel = testutil.GetRelation(t, 0, db, "db", schema.Name)
		txn.SetDedupType(txnif.FullSkipWorkSpaceDedup)
		err = rel.Append(context.Background(), bats[0])
		assert.NotNil(t, err)
		err = rel.Append(context.Background(), bats[1])
		assert.NotNil(t, err)

		err = rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{metaLoc1, metaLoc2})
		assert.NotNil(t, err)

		//check blk count.
		cntOfAblk := 0
		cntOfblk := 0
		testutil.ForEachBlock(rel, func(blk handle.Block) (err error) {
			if blk.IsAppendableBlock() {
				view, err := blk.GetColumnDataById(context.Background(), 3)
				assert.NoError(t, err)
				defer view.Close()
				cntOfAblk++
				return nil
			}
			metaLoc := blk.GetMetaLoc()
			assert.True(t, !metaLoc.IsEmpty())
			if bytes.Equal(metaLoc, metaLoc1) {
				view, err := blk.GetColumnDataById(context.Background(), 2)
				assert.NoError(t, err)
				defer view.Close()
				assert.True(t, view.GetData().Equals(bats[0].Vecs[2]))
			} else {
				view, err := blk.GetColumnDataById(context.Background(), 3)
				assert.NoError(t, err)
				defer view.Close()
				assert.True(t, view.GetData().Equals(bats[1].Vecs[3]))

			}
			cntOfblk++
			return
		})
		assert.True(t, cntOfblk == 2)
		assert.True(t, cntOfAblk == 2)
		assert.Nil(t, txn.Commit(context.Background()))

		//check count of committed segments.
		cntOfAseg := 0
		cntOfseg := 0
		txn, rel = testutil.GetRelation(t, 0, db, "db", schema.Name)
		testutil.ForEachSegment(rel, func(seg handle.Segment) (err error) {
			if seg.IsAppendable() {
				cntOfAseg++
				return
			}
			cntOfseg++
			return
		})
		assert.True(t, cntOfseg == 1)
		assert.True(t, cntOfAseg == 1)
		assert.Nil(t, txn.Commit(context.Background()))
	}
}

func TestCompactMemAlter(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()

	worker := ops.NewOpWorker(context.Background(), "xx")
	worker.Start()
	defer worker.Stop()
	schema := catalog.MockSchemaAll(5, 2)
	schema.BlockMaxRows = 20
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	testutil.CreateRelationAndAppend(t, 0, db, "db", schema, bat, true)

	// Alter: add a column to the last
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		err := rel.AlterTable(context.TODO(), api.NewAddColumnReq(0, 0, "xyz", types.NewProtoType(types.T_char), 5))
		require.NoError(t, err)
		require.Nil(t, txn.Commit(context.Background()))
	}
	var newBlockFp *common.ID
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		blkMeta := testutil.GetOneBlockMeta(rel)
		// ablk-0 & nablk-1
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta, db.Runtime)
		assert.NoError(t, err)
		worker.SendOp(task)
		err = task.WaitDone()
		assert.NoError(t, err)
		newBlockFp = task.GetNewBlock().Fingerprint()
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		seg, err := rel.GetSegment(newBlockFp.SegmentID())
		assert.Nil(t, err)
		blk, err := seg.GetBlock(newBlockFp.BlockID)
		assert.Nil(t, err)
		for i := 0; i <= 5; i++ {
			view, err := blk.GetColumnDataById(context.Background(), i)
			assert.NoError(t, err)
			defer view.Close()
			if i < 5 {
				require.Equal(t, bat.Vecs[i].GetType().Oid, view.GetData().GetType().Oid)
			} else {
				require.Equal(t, types.T_char.ToType().Oid, view.GetData().GetType().Oid)
			}
			if i == 3 {
				assert.True(t, view.GetData().Equals(bat.Vecs[3]))
			}
		}
		require.NoError(t, txn.Commit(context.Background()))
	}
}

func TestCompactBlock2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()

	worker := ops.NewOpWorker(context.Background(), "xx")
	worker.Start()
	defer worker.Stop()
	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 20
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	testutil.CreateRelationAndAppend(t, 0, db, "db", schema, bat, true)
	var newBlockFp *common.ID
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		blkMeta := testutil.GetOneBlockMeta(rel)
		// ablk-0 & nablk-1
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blkMeta, db.Runtime)
		assert.NoError(t, err)
		worker.SendOp(task)
		err = task.WaitDone()
		assert.NoError(t, err)
		newBlockFp = task.GetNewBlock().Fingerprint()
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		t.Log(db.Catalog.SimplePPString(common.PPL1))
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		t.Log(rel.SimplePPString(common.PPL1))
		seg, err := rel.GetSegment(newBlockFp.SegmentID())
		assert.Nil(t, err)
		blk, err := seg.GetBlock(newBlockFp.BlockID)
		assert.Nil(t, err)
		view, err := blk.GetColumnDataById(context.Background(), 3)
		assert.NoError(t, err)
		defer view.Close()
		assert.True(t, view.GetData().Equals(bat.Vecs[3]))
		// delete two rows on nablk-1
		err = blk.RangeDelete(1, 2, handle.DT_Normal)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}

	// Alter: add a column
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		rel.AlterTable(context.TODO(), api.NewAddColumnReq(0, 0, "xyz", types.NewProtoType(types.T_char), 3))
		assert.Nil(t, txn.Commit(context.Background()))
	}

	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		seg, err := rel.GetSegment(newBlockFp.SegmentID())
		assert.Nil(t, err)
		blk, err := seg.GetBlock(newBlockFp.BlockID)
		assert.Nil(t, err)

		// read generated column from nablk-1
		newColumnView, err := blk.GetColumnDataById(context.Background(), 3)
		require.NoError(t, err)
		require.Equal(t, 2, newColumnView.DeleteMask.GetCardinality())
		require.Equal(t, 20, newColumnView.GetData().Length())
		newData := newColumnView.ApplyDeletes()
		cnt := 0
		newData.Foreach(func(v any, isNull bool, row int) error {
			require.True(t, isNull)
			cnt++
			return nil
		}, nil)
		require.Equal(t, 18, cnt)

		// new nablk-2 18 rows
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blk.GetMeta().(*catalog.BlockEntry), db.Runtime)
		assert.Nil(t, err)
		worker.SendOp(task)
		err = task.WaitDone()
		assert.Nil(t, err)
		newBlockFp = task.GetNewBlock().Fingerprint()
		assert.Nil(t, txn.Commit(context.Background()))
	}
	{
		t.Log(db.Catalog.SimplePPString(common.PPL1))
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		seg, err := rel.GetSegment(newBlockFp.SegmentID())
		assert.Nil(t, err)
		blk, err := seg.GetBlock(newBlockFp.BlockID)
		assert.Nil(t, err)
		// not generated, it is a new column produced in sort(shuffle) process of the previous compaction
		view, err := blk.GetColumnDataById(context.Background(), 3)
		require.False(t, view.GetData().GetDownstreamVector().IsConstNull())
		cnt := 0
		view.GetData().Foreach(func(v any, isNull bool, row int) error {
			require.True(t, isNull)
			cnt++
			return nil
		}, nil)
		require.Equal(t, 18, cnt)
		assert.Nil(t, err)
		defer view.Close()
		assert.Nil(t, view.DeleteMask)
		// t.Logf("view: %v", view.GetData().String())
		// t.Logf("raw : %v", bat.Vecs[3].String())
		assert.Equal(t, bat.Vecs[0].Length()-2, view.Length())

		cnt = 0
		testutil.ForEachBlock(rel, func(blk handle.Block) (err error) {
			cnt++
			return
		})
		assert.Equal(t, 1, cnt)

		// this compaction create a nablk-3 having same data with the nablk-2
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blk.GetMeta().(*catalog.BlockEntry), db.Runtime)
		assert.Nil(t, err)
		worker.SendOp(task)
		err = task.WaitDone()
		assert.Nil(t, err)
		newBlockFp = task.GetNewBlock().Fingerprint()
		{
			txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
			seg, err := rel.GetSegment(newBlockFp.SegmentID())
			assert.NoError(t, err)
			blk, err := seg.GetBlock(newBlockFp.BlockID)
			assert.NoError(t, err)
			// delete two rows on nablk-3
			err = blk.RangeDelete(4, 5, handle.DT_Normal)
			assert.NoError(t, err)
			assert.NoError(t, txn.Commit(context.Background()))
		}
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		t.Log(rel.SimplePPString(common.PPL1))
		t.Log(db.Catalog.SimplePPString(common.PPL1))
		seg, err := rel.GetSegment(newBlockFp.SegmentID())
		assert.Nil(t, err)
		blk, err := seg.GetBlock(newBlockFp.BlockID)
		assert.Nil(t, err)
		view, err := blk.GetColumnDataById(context.Background(), 3)
		assert.Nil(t, err)
		defer view.Close()
		assert.True(t, view.DeleteMask.Contains(4))
		assert.True(t, view.DeleteMask.Contains(5))
		assert.Equal(t, bat.Vecs[0].Length()-2, view.Length())

		// this delete will be transfered to nablk-4
		txn2, rel2 := testutil.GetDefaultRelation(t, db, schema.Name)
		seg2, err := rel2.GetSegment(newBlockFp.SegmentID())
		assert.NoError(t, err)
		blk2, err := seg2.GetBlock(newBlockFp.BlockID)
		assert.NoError(t, err)
		err = blk2.RangeDelete(7, 7, handle.DT_Normal)
		assert.NoError(t, err)

		// new nablk-4 16 rows
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, blk.GetMeta().(*catalog.BlockEntry), db.Runtime)
		assert.NoError(t, err)
		worker.SendOp(task)
		err = task.WaitDone()
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
		// newBlockFp = task.GetNewBlock().Fingerprint()

		err = txn2.Commit(context.Background())

		// nablk-4 has 15 rows
		assert.NoError(t, err)
	}

	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		newSchema := rel.Schema().(*catalog.Schema)
		bats := catalog.MockBatch(newSchema, 30).Split(3)
		err := rel.Append(context.Background(), bats[0]) // duplicate
		require.Error(t, err)
		t.Log(err)
		err = rel.Append(context.Background(), bats[2]) // create ablk-5
		require.NoError(t, err)

		cnt := 0
		testutil.ForEachBlock(rel, func(blk handle.Block) (err error) {
			cnt++
			return
		})
		require.Equal(t, 2, cnt)
		require.NoError(t, txn.Commit(context.Background()))
	}
	// Alter: add new column and remove previous added column
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		newSchema := rel.Schema().(*catalog.Schema)
		// remove xyz
		rel.AlterTable(context.TODO(), api.NewRemoveColumnReq(0, 0, 3, uint32(newSchema.ColDefs[3].SeqNum)))
		// add uvw at the front
		rel.AlterTable(context.TODO(), api.NewAddColumnReq(0, 0, "uvw", types.NewProtoType(types.T_int32), 0))
		assert.Nil(t, txn.Commit(context.Background()))
	}

	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		newSchema := rel.Schema().(*catalog.Schema)
		bats := catalog.MockBatch(newSchema, 40).Split(4)
		err := rel.Append(context.Background(), bats[0]) // duplicate with nablk-4
		require.Error(t, err)
		t.Log(err)
		err = rel.Append(context.Background(), bats[2]) // duplicate with ablk-5
		require.Error(t, err)
		t.Log(err)
		err = rel.Append(context.Background(), bats[3]) // create ablk-6
		require.NoError(t, err)
		cnt := 0
		testutil.ForEachBlock(rel, func(blk handle.Block) error {
			id := blk.ID()
			t.Log(cnt, id.String())
			switch cnt {
			case 0:
				// localseg blk, will be ablk-6
				view, err := blk.GetColumnDataById(context.Background(), 0 /*uvw*/)
				require.NoError(t, err)
				require.Equal(t, 10, view.Length())
				require.False(t, view.GetData().GetDownstreamVector().IsConstNull())
				require.Equal(t, types.T_int32, view.GetData().GetType().Oid)
				view.GetData().Foreach(func(v any, isNull bool, row int) error { require.False(t, isNull); return nil }, nil)
			case 1:
				// nablk-4 16 rows + 1 delete
				require.False(t, blk.IsAppendableBlock())
				view, err := blk.GetColumnDataById(context.Background(), 0 /*uvw*/)
				require.NoError(t, err)
				require.Equal(t, types.T_int32, view.GetData().GetType().Oid)
				require.Equal(t, 16, view.Length())
				require.Equal(t, 1, view.DeleteMask.GetCardinality())
				require.True(t, view.GetData().GetDownstreamVector().IsConstNull())
			case 2:
				// ablk-5 10 rows
				view, err := blk.GetColumnDataById(context.Background(), 0 /*uvw*/)
				require.NoError(t, err)
				require.Equal(t, types.T_int32, view.GetData().GetType().Oid)
				require.Equal(t, 10, view.Length())
				require.Nil(t, view.DeleteMask)
				require.True(t, view.GetData().GetDownstreamVector().IsConstNull())
			}
			cnt++
			return nil
		})
		require.Equal(t, 3, cnt)
		assert.Nil(t, txn.Commit(context.Background()))
	}
	t.Log(db.Catalog.SimplePPString(common.PPL1))
}

func TestAutoCompactABlk1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 3)
	schema.BlockMaxRows = 1000
	schema.SegmentMaxBlocks = 10

	totalRows := schema.BlockMaxRows / 5
	bat := catalog.MockBatch(schema, int(totalRows))
	defer bat.Close()
	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)
	time.Sleep(time.Millisecond * 2)
	testutils.WaitExpect(5000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	assert.Equal(t, uint64(0), tae.Runtime.Scheduler.GetPenddingLSNCnt())
	t.Log(tae.Catalog.SimplePPString(common.PPL1))
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		blk := testutil.GetOneBlock(rel)
		blkData := blk.GetMeta().(*catalog.BlockEntry).GetBlockData()
		factory, taskType, scopes, err := blkData.BuildCompactionTaskFactory()
		assert.Nil(t, err)
		task, err := tae.Runtime.Scheduler.ScheduleMultiScopedTxnTask(tasks.WaitableCtx, taskType, scopes, factory)
		assert.Nil(t, err)
		err = task.WaitDone()
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}
}

func TestAutoCompactABlk2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := new(options.Options)
	opts.CacheCfg = new(options.CacheCfg)
	opts = config.WithQuickScanAndCKPOpts(opts)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()

	schema1 := catalog.MockSchemaAll(13, 2)
	schema1.BlockMaxRows = 5
	schema1.SegmentMaxBlocks = 2

	schema2 := catalog.MockSchemaAll(13, 2)
	schema2.BlockMaxRows = 5
	schema2.SegmentMaxBlocks = 2
	{
		txn, _ := db.StartTxn(nil)
		database, err := txn.CreateDatabase("db", "", "")
		assert.Nil(t, err)
		_, err = database.CreateRelation(schema1)
		assert.Nil(t, err)
		_, err = database.CreateRelation(schema2)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}
	bat := catalog.MockBatch(schema1, int(schema1.BlockMaxRows*3-1))
	defer bat.Close()
	bats := bat.Split(bat.Length())

	pool, err := ants.NewPool(20)
	assert.Nil(t, err)
	defer pool.Release()
	var wg sync.WaitGroup
	doSearch := func(name string) func() {
		return func() {
			defer wg.Done()
			txn, rel := testutil.GetDefaultRelation(t, db, name)
			it := rel.MakeBlockIt()
			for it.Valid() {
				blk := it.GetBlock()
				view, err := blk.GetColumnDataById(context.Background(), schema1.GetSingleSortKeyIdx())
				assert.Nil(t, err)
				view.Close()
				it.Next()
			}
			err := txn.Commit(context.Background())
			assert.NoError(t, err)
		}
	}

	for _, data := range bats {
		wg.Add(4)
		err := pool.Submit(doSearch(schema1.Name))
		assert.Nil(t, err)
		err = pool.Submit(doSearch(schema2.Name))
		assert.Nil(t, err)
		err = pool.Submit(testutil.AppendClosure(t, data, schema1.Name, db, &wg))
		assert.Nil(t, err)
		err = pool.Submit(testutil.AppendClosure(t, data, schema2.Name, db, &wg))
		assert.Nil(t, err)
	}
	wg.Wait()
	testutils.WaitExpect(20000, func() bool {
		return db.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	assert.Equal(t, uint64(0), db.Runtime.Scheduler.GetPenddingLSNCnt())
	t.Log(db.Catalog.SimplePPString(common.PPL1))
	t.Logf("GetPenddingLSNCnt: %d", db.Runtime.Scheduler.GetPenddingLSNCnt())
	t.Logf("GetCheckpointed: %d", db.Runtime.Scheduler.GetCheckpointedLSN())
}

func TestCompactABlk(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 3)
	schema.BlockMaxRows = 1000
	schema.SegmentMaxBlocks = 10

	totalRows := schema.BlockMaxRows / 5
	bat := catalog.MockBatch(schema, int(totalRows))
	defer bat.Close()
	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		blk := testutil.GetOneBlock(rel)
		blkData := blk.GetMeta().(*catalog.BlockEntry).GetBlockData()
		factory, taskType, scopes, err := blkData.BuildCompactionTaskFactory()
		assert.NoError(t, err)
		task, err := tae.Runtime.Scheduler.ScheduleMultiScopedTxnTask(tasks.WaitableCtx, taskType, scopes, factory)
		assert.NoError(t, err)
		err = task.WaitDone()
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	err := tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.StatMaxCommitTS())
	assert.NoError(t, err)
	lsn := tae.BGCheckpointRunner.MaxLSNInRange(tae.TxnMgr.StatMaxCommitTS())
	entry, err := tae.Wal.RangeCheckpoint(1, lsn)
	assert.NoError(t, err)
	assert.NoError(t, entry.WaitDone())
	testutils.WaitExpect(1000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	assert.Equal(t, uint64(0), tae.Runtime.Scheduler.GetPenddingLSNCnt())
	t.Log(tae.Catalog.SimplePPString(common.PPL1))
}

func TestRollback1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	db := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer db.Close()
	schema := catalog.MockSchema(2, 0)

	testutil.CreateRelation(t, db, "db", schema, true)

	segCnt := 0
	onSegFn := func(segment *catalog.SegmentEntry) error {
		segCnt++
		return nil
	}
	blkCnt := 0
	onBlkFn := func(block *catalog.BlockEntry) error {
		blkCnt++
		return nil
	}
	processor := new(catalog.LoopProcessor)
	processor.SegmentFn = onSegFn
	processor.BlockFn = onBlkFn
	txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
	_, err := rel.CreateSegment(false)
	assert.Nil(t, err)

	tableMeta := rel.GetMeta().(*catalog.TableEntry)
	err = tableMeta.RecurLoop(processor)
	assert.Nil(t, err)
	assert.Equal(t, segCnt, 1)

	assert.Nil(t, txn.Rollback(context.Background()))
	segCnt = 0
	err = tableMeta.RecurLoop(processor)
	assert.Nil(t, err)
	assert.Equal(t, segCnt, 0)

	txn, rel = testutil.GetDefaultRelation(t, db, schema.Name)
	seg, err := rel.CreateSegment(false)
	assert.Nil(t, err)
	segMeta := seg.GetMeta().(*catalog.SegmentEntry)
	assert.Nil(t, txn.Commit(context.Background()))
	segCnt = 0
	err = tableMeta.RecurLoop(processor)
	assert.Nil(t, err)
	assert.Equal(t, segCnt, 1)

	txn, rel = testutil.GetDefaultRelation(t, db, schema.Name)
	seg, err = rel.GetSegment(&segMeta.ID)
	assert.Nil(t, err)
	_, err = seg.CreateBlock(false)
	assert.Nil(t, err)
	blkCnt = 0
	err = tableMeta.RecurLoop(processor)
	assert.Nil(t, err)
	assert.Equal(t, blkCnt, 1)

	err = txn.Rollback(context.Background())
	assert.Nil(t, err)
	blkCnt = 0
	err = tableMeta.RecurLoop(processor)
	assert.Nil(t, err)
	assert.Equal(t, blkCnt, 0)

	t.Log(db.Catalog.SimplePPString(common.PPL1))
}

func TestMVCC1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	db := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer db.Close()
	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 40
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*10))
	defer bat.Close()
	bats := bat.Split(40)

	txn, _, rel := testutil.CreateRelationNoCommit(t, db, "db", schema, true)
	err := rel.Append(context.Background(), bats[0])
	assert.NoError(t, err)

	row := 5
	expectVal := bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(row)
	filter := handle.NewEQFilter(expectVal)
	actualVal, _, err := rel.GetValueByFilter(context.Background(), filter, schema.GetSingleSortKeyIdx())
	assert.NoError(t, err)
	assert.Equal(t, expectVal, actualVal)
	assert.NoError(t, txn.Commit(context.Background()))

	_, rel = testutil.GetDefaultRelation(t, db, schema.Name)
	actualVal, _, err = rel.GetValueByFilter(context.Background(), filter, schema.GetSingleSortKeyIdx())
	assert.NoError(t, err)
	assert.Equal(t, expectVal, actualVal)

	txn2, rel2 := testutil.GetDefaultRelation(t, db, schema.Name)
	err = rel2.Append(context.Background(), bats[1])
	assert.NoError(t, err)

	val2 := bats[1].Vecs[schema.GetSingleSortKeyIdx()].Get(row)
	filter.Val = val2
	actualVal, _, err = rel2.GetValueByFilter(context.Background(), filter, schema.GetSingleSortKeyIdx())
	assert.NoError(t, err)
	assert.Equal(t, val2, actualVal)

	assert.NoError(t, txn2.Commit(context.Background()))

	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.Error(t, err)
	var id *common.ID

	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		id, _, err = rel.GetByFilter(context.Background(), filter)
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}

	it := rel.MakeBlockIt()
	for it.Valid() {
		block := it.GetBlock()
		bid := block.Fingerprint()
		if bid.BlockID == id.BlockID {
			view, err := block.GetColumnDataById(context.Background(), schema.GetSingleSortKeyIdx())
			assert.Nil(t, err)
			defer view.Close()
			assert.Nil(t, view.DeleteMask)
			assert.NotNil(t, view.GetData())
			t.Log(view.GetData().String())
			assert.Equal(t, bats[0].Vecs[0].Length(), view.Length())
		}
		it.Next()
	}
}

// 1. Txn1 create db, relation and append 10 rows. committed -- PASS
// 2. Txn2 append 10 rows. Get the 5th append row value -- PASS
// 3. Txn2 delete the 5th row value in uncommitted state -- PASS
// 4. Txn2 get the 5th row value -- NotFound
func TestMVCC2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	db := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer db.Close()
	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	bats := bat.Split(10)
	{
		txn, _, rel := testutil.CreateRelationNoCommit(t, db, "db", schema, true)
		err := rel.Append(context.Background(), bats[0])
		assert.NoError(t, err)
		val := bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(5)
		filter := handle.NewEQFilter(val)
		_, _, err = rel.GetByFilter(context.Background(), filter)
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		err := rel.Append(context.Background(), bats[1])
		assert.NoError(t, err)
		val := bats[1].Vecs[schema.GetSingleSortKeyIdx()].Get(5)
		filter := handle.NewEQFilter(val)
		err = rel.DeleteByFilter(context.Background(), filter)
		assert.NoError(t, err)

		_, _, err = rel.GetByFilter(context.Background(), filter)
		assert.Error(t, err)
		t.Log(err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		it := rel.MakeBlockIt()
		for it.Valid() {
			block := it.GetBlock()
			view, err := block.GetColumnDataByName(context.Background(), schema.GetSingleSortKey().Name)
			assert.Nil(t, err)
			assert.Nil(t, view.DeleteMask)
			assert.Equal(t, bats[1].Vecs[0].Length()*2-1, view.Length())
			// TODO: exclude deleted rows when apply appends
			it.Next()
			view.Close()
		}
		assert.NoError(t, txn.Commit(context.Background()))
	}
}

func TestUnload1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := new(options.Options)
	opts.CacheCfg = new(options.CacheCfg)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()

	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2

	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*2))
	defer bat.Close()
	bats := bat.Split(int(schema.BlockMaxRows))
	testutil.CreateRelation(t, db, "db", schema, true)
	var wg sync.WaitGroup
	pool, err := ants.NewPool(1)
	assert.Nil(t, err)
	defer pool.Release()
	for _, data := range bats {
		wg.Add(1)
		err := pool.Submit(testutil.AppendClosure(t, data, schema.Name, db, &wg))
		assert.Nil(t, err)
	}
	wg.Wait()
	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema.Name)
		for i := 0; i < 10; i++ {
			it := rel.MakeBlockIt()
			for it.Valid() {
				blk := it.GetBlock()
				view, err := blk.GetColumnDataByName(context.Background(), schema.GetSingleSortKey().Name)
				assert.Nil(t, err)
				defer view.Close()
				assert.Equal(t, int(schema.BlockMaxRows), view.Length())
				it.Next()
			}
		}
		_ = txn.Commit(context.Background())
	}
}

func TestUnload2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := new(options.Options)
	opts.CacheCfg = new(options.CacheCfg)
	db := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer db.Close()

	schema1 := catalog.MockSchemaAll(13, 2)
	schema1.BlockMaxRows = 10
	schema1.SegmentMaxBlocks = 2

	schema2 := catalog.MockSchemaAll(13, 2)
	schema2.BlockMaxRows = 10
	schema2.SegmentMaxBlocks = 2
	{
		txn, _ := db.StartTxn(nil)
		database, err := txn.CreateDatabase("db", "", "")
		assert.Nil(t, err)
		_, err = database.CreateRelation(schema1)
		assert.Nil(t, err)
		_, err = database.CreateRelation(schema2)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}

	bat := catalog.MockBatch(schema1, int(schema1.BlockMaxRows*5+5))
	defer bat.Close()
	bats := bat.Split(bat.Length())

	p, err := ants.NewPool(10)
	assert.Nil(t, err)
	defer p.Release()
	var wg sync.WaitGroup
	for i, data := range bats {
		wg.Add(1)
		name := schema1.Name
		if i%2 == 1 {
			name = schema2.Name
		}
		err := p.Submit(testutil.AppendClosure(t, data, name, db, &wg))
		assert.Nil(t, err)
	}
	wg.Wait()

	{
		txn, rel := testutil.GetDefaultRelation(t, db, schema1.Name)
		for i := 0; i < len(bats); i += 2 {
			data := bats[i]
			v := data.Vecs[schema1.GetSingleSortKeyIdx()].Get(0)
			filter := handle.NewEQFilter(v)
			_, _, err := rel.GetByFilter(context.Background(), filter)
			assert.NoError(t, err)
		}
		database, _ := txn.GetDatabase("db")
		rel, err = database.GetRelationByName(schema2.Name)
		assert.Nil(t, err)
		for i := 1; i < len(bats); i += 2 {
			data := bats[i]
			v := data.Vecs[schema1.GetSingleSortKeyIdx()].Get(0)
			filter := handle.NewEQFilter(v)
			_, _, err := rel.GetByFilter(context.Background(), filter)
			assert.NoError(t, err)
		}
		_ = txn.Commit(context.Background())
	}
}

func TestDelete1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()

	schema := catalog.MockSchemaAll(3, 2)
	schema.BlockMaxRows = 10
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)
	var id *common.ID
	var row uint32
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		assert.Equal(t, bat.Length(), int(rel.Rows()))
		pkCol := bat.Vecs[schema.GetSingleSortKeyIdx()]
		pkVal := pkCol.Get(5)
		filter := handle.NewEQFilter(pkVal)
		var err error
		id, row, err = rel.GetByFilter(context.Background(), filter)
		assert.NoError(t, err)
		err = rel.RangeDelete(id, row, row, handle.DT_Normal)
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		assert.Equal(t, bat.Length()-1, int(rel.Rows()))
		pkCol := bat.Vecs[schema.GetSingleSortKeyIdx()]
		pkVal := pkCol.Get(5)
		filter := handle.NewEQFilter(pkVal)
		_, _, err := rel.GetByFilter(context.Background(), filter)
		assert.Error(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		blkMeta := testutil.GetOneBlockMeta(rel)
		blkData := blkMeta.GetBlockData()
		factory, taskType, scopes, err := blkData.BuildCompactionTaskFactory()
		assert.NoError(t, err)
		task, err := tae.Runtime.Scheduler.ScheduleMultiScopedTxnTask(tasks.WaitableCtx, taskType, scopes, factory)
		assert.NoError(t, err)
		err = task.WaitDone()
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		blk := testutil.GetOneBlock(rel)
		view, err := blk.GetColumnDataById(context.Background(), schema.GetSingleSortKeyIdx())
		assert.NoError(t, err)
		defer view.Close()
		assert.Nil(t, view.DeleteMask)
		assert.Equal(t, bat.Vecs[0].Length()-1, view.Length())

		err = blk.RangeDelete(0, 0, handle.DT_Normal)
		assert.NoError(t, err)
		view, err = blk.GetColumnDataById(context.Background(), schema.GetSingleSortKeyIdx())
		assert.NoError(t, err)
		defer view.Close()
		assert.True(t, view.DeleteMask.Contains(0))
		v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(0)
		filter := handle.NewEQFilter(v)
		_, _, err = rel.GetByFilter(context.Background(), filter)
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		assert.NoError(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		assert.Equal(t, bat.Length()-2, int(rel.Rows()))
		blk := testutil.GetOneBlock(rel)
		view, err := blk.GetColumnDataById(context.Background(), schema.GetSingleSortKeyIdx())
		assert.NoError(t, err)
		defer view.Close()
		assert.True(t, view.DeleteMask.Contains(0))
		assert.Equal(t, bat.Vecs[0].Length()-1, view.Length())
		v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(0)
		filter := handle.NewEQFilter(v)
		_, _, err = rel.GetByFilter(context.Background(), filter)
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		_ = txn.Rollback(context.Background())
	}
	t.Log(tae.Catalog.SimplePPString(common.PPL1))
}

func TestLogIndex1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 0)
	schema.BlockMaxRows = 10
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	bats := bat.Split(int(schema.BlockMaxRows))
	testutil.CreateRelation(t, tae, "db", schema, true)
	txns := make([]txnif.AsyncTxn, 0)
	doAppend := func(data *containers.Batch) func() {
		return func() {
			txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
			err := rel.Append(context.Background(), data)
			assert.NoError(t, err)
			assert.NoError(t, txn.Commit(context.Background()))
			txns = append(txns, txn)
		}
	}
	for _, data := range bats {
		doAppend(data)()
	}
	var id *common.ID
	var offset uint32
	var err error
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
		filter := handle.NewEQFilter(v)
		id, offset, err = rel.GetByFilter(context.Background(), filter)
		assert.Nil(t, err)
		err = rel.RangeDelete(id, offset, offset, handle.DT_Normal)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}
	{
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		blk := testutil.GetOneBlock(rel)
		meta := blk.GetMeta().(*catalog.BlockEntry)

		view, err := blk.GetColumnDataById(context.Background(), schema.GetSingleSortKeyIdx())
		assert.Nil(t, err)
		defer view.Close()
		assert.True(t, view.DeleteMask.Contains(uint64(offset)))
		task, err := jobs.NewCompactBlockTask(nil, txn, meta, tae.Runtime)
		assert.Nil(t, err)
		err = task.OnExec(context.Background())
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}
}

func TestCrossDBTxn(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()

	txn, _ := tae.StartTxn(nil)
	db1, err := txn.CreateDatabase("db1", "", "")
	assert.Nil(t, err)
	db2, err := txn.CreateDatabase("db2", "", "")
	assert.Nil(t, err)
	assert.NotNil(t, db1)
	assert.NotNil(t, db2)
	assert.Nil(t, txn.Commit(context.Background()))

	schema1 := catalog.MockSchema(2, 0)
	schema1.BlockMaxRows = 10
	schema1.SegmentMaxBlocks = 2
	schema2 := catalog.MockSchema(4, 0)
	schema2.BlockMaxRows = 10
	schema2.SegmentMaxBlocks = 2

	rows1 := schema1.BlockMaxRows * 5 / 2
	rows2 := schema1.BlockMaxRows * 3 / 2
	bat1 := catalog.MockBatch(schema1, int(rows1))
	bat2 := catalog.MockBatch(schema2, int(rows2))
	defer bat1.Close()
	defer bat2.Close()

	txn, _ = tae.StartTxn(nil)
	db1, err = txn.GetDatabase("db1")
	assert.Nil(t, err)
	db2, err = txn.GetDatabase("db2")
	assert.Nil(t, err)
	rel1, err := db1.CreateRelation(schema1)
	assert.Nil(t, err)
	rel2, err := db2.CreateRelation(schema2)
	assert.Nil(t, err)
	err = rel1.Append(context.Background(), bat1)
	assert.Nil(t, err)
	err = rel2.Append(context.Background(), bat2)
	assert.Nil(t, err)

	assert.Nil(t, txn.Commit(context.Background()))

	txn, _ = tae.StartTxn(nil)
	db1, err = txn.GetDatabase("db1")
	assert.NoError(t, err)
	db2, err = txn.GetDatabase("db2")
	assert.NoError(t, err)
	rel1, err = db1.GetRelationByName(schema1.Name)
	assert.NoError(t, err)
	rel2, err = db2.GetRelationByName(schema2.Name)
	assert.NoError(t, err)

	testutil.CheckAllColRowsByScan(t, rel1, int(rows1), false)
	testutil.CheckAllColRowsByScan(t, rel2, int(rows2), false)

	t.Log(tae.Catalog.SimplePPString(common.PPL1))
}

func TestSystemDB1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchema(2, 0)
	txn, _ := tae.StartTxn(nil)
	_, err := txn.CreateDatabase(pkgcatalog.MO_CATALOG, "", "")
	assert.NotNil(t, err)
	_, err = txn.DropDatabase(pkgcatalog.MO_CATALOG)
	assert.NotNil(t, err)

	db1, err := txn.CreateDatabase("db1", "", "")
	assert.Nil(t, err)
	_, err = db1.CreateRelation(schema)
	assert.Nil(t, err)

	_, err = txn.CreateDatabase("db2", "", "")
	assert.Nil(t, err)

	db, _ := txn.GetDatabase(pkgcatalog.MO_CATALOG)
	table, err := db.GetRelationByName(pkgcatalog.MO_DATABASE)
	assert.Nil(t, err)
	it := table.MakeBlockIt()
	rows := 0
	for it.Valid() {
		blk := it.GetBlock()
		rows += blk.Rows()
		view, err := blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemDBAttr_Name)
		assert.Nil(t, err)
		defer view.Close()
		assert.Equal(t, 3, view.Length())
		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemDBAttr_CatalogName)
		assert.Nil(t, err)
		defer view.Close()
		assert.Equal(t, 3, view.Length())
		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemDBAttr_CreateSQL)
		assert.Nil(t, err)
		defer view.Close()
		assert.Equal(t, 3, view.Length())
		it.Next()
	}
	assert.Equal(t, 3, rows)

	table, err = db.GetRelationByName(pkgcatalog.MO_TABLES)
	assert.Nil(t, err)
	it = table.MakeBlockIt()
	rows = 0
	for it.Valid() {
		blk := it.GetBlock()
		rows += blk.Rows()
		view, err := blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemRelAttr_Name)
		assert.Nil(t, err)
		defer view.Close()
		assert.Equal(t, 4, view.Length())
		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemRelAttr_Persistence)
		assert.NoError(t, err)
		defer view.Close()
		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemRelAttr_Kind)
		assert.NoError(t, err)
		defer view.Close()
		it.Next()
	}
	assert.Equal(t, 4, rows)

	table, err = db.GetRelationByName(pkgcatalog.MO_COLUMNS)
	assert.Nil(t, err)

	bat := containers.NewBatch()
	defer bat.Close()
	// schema2 := table.GetMeta().(*catalog.TableEntry).GetSchema()
	// bat := containers.BuildBatch(schema2.AllNames(), schema2.AllTypes(), schema2.AllNullables(), 0)
	it = table.MakeBlockIt()
	rows = 0
	for it.Valid() {
		blk := it.GetBlock()
		rows += blk.Rows()
		view, err := blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemColAttr_DBName)
		assert.NoError(t, err)
		defer view.Close()
		bat.AddVector(pkgcatalog.SystemColAttr_DBName, view.Orphan())

		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemColAttr_RelName)
		assert.Nil(t, err)
		defer view.Close()
		bat.AddVector(pkgcatalog.SystemColAttr_RelName, view.Orphan())

		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemColAttr_Name)
		assert.Nil(t, err)
		defer view.Close()
		bat.AddVector(pkgcatalog.SystemColAttr_Name, view.Orphan())

		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemColAttr_ConstraintType)
		assert.Nil(t, err)
		defer view.Close()
		t.Log(view.GetData().String())
		bat.AddVector(pkgcatalog.SystemColAttr_ConstraintType, view.Orphan())

		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemColAttr_Type)
		assert.Nil(t, err)
		defer view.Close()
		t.Log(view.GetData().String())
		view, err = blk.GetColumnDataByName(context.Background(), pkgcatalog.SystemColAttr_Num)
		assert.Nil(t, err)
		defer view.Close()
		t.Log(view.GetData().String())
		it.Next()
	}
	t.Log(rows)

	for i := 0; i < bat.Vecs[0].Length(); i++ {
		dbName := string(bat.Vecs[0].Get(i).([]byte))
		relName := string(bat.Vecs[1].Get(i).([]byte))
		attrName := string(bat.Vecs[2].Get(i).([]byte))
		ct := string(bat.Vecs[3].Get(i).([]byte))
		if dbName == pkgcatalog.MO_CATALOG {
			if relName == pkgcatalog.MO_DATABASE {
				if attrName == pkgcatalog.SystemDBAttr_ID {
					assert.Equal(t, pkgcatalog.SystemColPKConstraint, ct)
				} else {
					assert.Equal(t, pkgcatalog.SystemColNoConstraint, ct)
				}
			} else if relName == pkgcatalog.MO_TABLES {
				if attrName == pkgcatalog.SystemRelAttr_ID {
					assert.Equal(t, pkgcatalog.SystemColPKConstraint, ct)
				} else {
					assert.Equal(t, pkgcatalog.SystemColNoConstraint, ct)
				}
			} else if relName == pkgcatalog.MO_COLUMNS {
				if attrName == pkgcatalog.SystemColAttr_UniqName {
					assert.Equal(t, pkgcatalog.SystemColPKConstraint, ct)
				} else {
					assert.Equal(t, pkgcatalog.SystemColNoConstraint, ct)
				}
			}
		}
	}

	err = txn.Rollback(context.Background())
	assert.Nil(t, err)
	t.Log(tae.Catalog.SimplePPString(common.PPL1))
}

func TestSystemDB2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()

	txn, _ := tae.StartTxn(nil)
	sysDB, err := txn.GetDatabase(pkgcatalog.MO_CATALOG)
	assert.NoError(t, err)
	_, err = sysDB.DropRelationByName(pkgcatalog.MO_DATABASE)
	assert.Error(t, err)
	_, err = sysDB.DropRelationByName(pkgcatalog.MO_TABLES)
	assert.Error(t, err)
	_, err = sysDB.DropRelationByName(pkgcatalog.MO_COLUMNS)
	assert.Error(t, err)

	schema := catalog.MockSchema(2, 0)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, 1000)
	defer bat.Close()

	rel, err := sysDB.CreateRelation(schema)
	assert.NoError(t, err)
	assert.NotNil(t, rel)
	err = rel.Append(context.Background(), bat)
	assert.Nil(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, _ = tae.StartTxn(nil)
	sysDB, err = txn.GetDatabase(pkgcatalog.MO_CATALOG)
	assert.NoError(t, err)
	rel, err = sysDB.GetRelationByName(schema.Name)
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, 1000, false)
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestSystemDB3(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()
	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	txn, _ := tae.StartTxn(nil)
	schema := catalog.MockSchemaAll(13, 12)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, 20)
	defer bat.Close()
	db, err := txn.GetDatabase(pkgcatalog.MO_CATALOG)
	assert.NoError(t, err)
	rel, err := db.CreateRelation(schema)
	assert.NoError(t, err)
	err = rel.Append(context.Background(), bat)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestScan1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()
	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()

	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 2

	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows-1))
	defer bat.Close()
	txn, _, rel := testutil.CreateRelationNoCommit(t, tae, testutil.DefaultTestDB, schema, true)
	err := rel.Append(context.Background(), bat)
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, bat.Length(), false)
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestDedup(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()

	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 2

	bat := catalog.MockBatch(schema, 10)
	defer bat.Close()
	txn, _, rel := testutil.CreateRelationNoCommit(t, tae, testutil.DefaultTestDB, schema, true)
	err := rel.Append(context.Background(), bat)
	assert.NoError(t, err)
	err = rel.Append(context.Background(), bat)
	t.Log(err)
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrDuplicateEntry))
	testutil.CheckAllColRowsByScan(t, rel, 10, false)
	err = txn.Rollback(context.Background())
	assert.NoError(t, err)
}

func TestScan2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 12)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 10
	rows := schema.BlockMaxRows * 5 / 2
	bat := catalog.MockBatch(schema, int(rows))
	defer bat.Close()
	bats := bat.Split(2)

	txn, _, rel := testutil.CreateRelationNoCommit(t, tae, testutil.DefaultTestDB, schema, true)
	err := rel.Append(context.Background(), bats[0])
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, bats[0].Length(), false)

	err = rel.Append(context.Background(), bats[0])
	assert.Error(t, err)
	err = rel.Append(context.Background(), bats[1])
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, int(rows), false)

	pkv := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(5)
	filter := handle.NewEQFilter(pkv)
	err = rel.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, int(rows)-1, true)

	pkv = bat.Vecs[schema.GetSingleSortKeyIdx()].Get(8)
	filter = handle.NewEQFilter(pkv)
	updateV := int64(999)
	err = rel.UpdateByFilter(context.Background(), filter, 3, updateV, false)
	assert.NoError(t, err)

	v, _, err := rel.GetValueByFilter(context.Background(), filter, 3)
	assert.NoError(t, err)
	assert.Equal(t, updateV, v.(int64))
	testutil.CheckAllColRowsByScan(t, rel, int(rows)-1, true)
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestADA(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 3)
	schema.BlockMaxRows = 1000
	bat := catalog.MockBatch(schema, 1)
	defer bat.Close()

	// Append to a block
	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)

	// Delete a row from the block
	txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
	v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(0)
	filter := handle.NewEQFilter(v)
	id, row, err := rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	// Append a row with the same primary key
	txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.Error(t, err)
	err = rel.Append(context.Background(), bat)
	assert.NoError(t, err)
	id, row, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, 1, true)

	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.Error(t, err)

	err = rel.Append(context.Background(), bat)
	assert.NoError(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	testutil.CheckAllColRowsByScan(t, rel, 1, true)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
	err = rel.Append(context.Background(), bat)
	assert.Error(t, err)
	id, row, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.Error(t, err)

	err = rel.Append(context.Background(), bat)
	assert.NoError(t, err)

	id, row, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)

	err = rel.Append(context.Background(), bat)
	assert.Error(t, err)

	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.Error(t, err)
	err = rel.Append(context.Background(), bat)
	assert.NoError(t, err)

	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
	err = rel.Append(context.Background(), bat)
	assert.Error(t, err)
	id, row, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.Error(t, err)

	err = rel.Append(context.Background(), bat)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = testutil.GetDefaultRelation(t, tae, schema.Name)
	it := rel.MakeBlockIt()
	for it.Valid() {
		blk := it.GetBlock()
		view, err := blk.GetColumnDataById(context.Background(), schema.GetSingleSortKeyIdx())
		assert.NoError(t, err)
		defer view.Close()
		assert.Equal(t, 4, view.Length())
		assert.Equal(t, 3, view.DeleteMask.GetCardinality())
		it.Next()
	}
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestUpdateByFilter(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 3)
	bat := catalog.MockBatch(schema, 100)
	defer bat.Close()

	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)

	txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
	v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(2)
	filter := handle.NewEQFilter(v)
	err := rel.UpdateByFilter(context.Background(), filter, 2, int32(2222), false)
	assert.NoError(t, err)

	id, row, err := rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	cv, _, err := rel.GetValue(id, row, 2)
	assert.NoError(t, err)
	assert.Equal(t, int32(2222), cv.(int32))

	v = bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter = handle.NewEQFilter(v)

	err = rel.UpdateByFilter(context.Background(), filter, uint16(schema.GetSingleSortKeyIdx()), int64(333333), false)
	assert.NoError(t, err)

	assert.NoError(t, txn.Commit(context.Background()))
}

// Test Steps
// 1. Create DB|Relation and append 10 rows. Commit
// 2. Make a equal filter with value of the pk of the second inserted row
// 3. Start Txn1. GetByFilter return PASS
// 4. Start Txn2. Delete row 2. Commit.
// 5. Txn1 call GetByFilter and should return PASS
func TestGetByFilter(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 12)
	bat := catalog.MockBatch(schema, 10)
	defer bat.Close()

	// Step 1
	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)

	// Step 2
	v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(2)
	filter := handle.NewEQFilter(v)

	// Step 3
	txn1, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
	id, row, err := rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)

	// Step 4
	{
		txn2, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		err := rel.RangeDelete(id, row, row, handle.DT_Normal)
		assert.NoError(t, err)
		assert.NoError(t, txn2.Commit(context.Background()))
	}

	// Step 5
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	assert.NoError(t, txn1.Commit(context.Background()))
}

//  1. Set a big BlockMaxRows
//  2. Mock one row batch
//  3. Start tones of workers. Each work execute below routines:
//     3.1 GetByFilter a pk val
//     3.1.1 If found, go to 3.5
//     3.2 Append a row
//     3.3 err should not be duplicated(TODO: now is duplicated, should be W-W conflict)
//     (why not duplicated: previous GetByFilter had checked that there was no duplicate key)
//     3.4 If no error. try commit. If commit ok, inc appendedcnt. If error, rollback
//     3.5 Delete the row
//     3.5.1 If no error. try commit. commit should always pass
//     3.5.2 If error, should always be w-w conflict
//  4. Wait done all workers. Check the raw row count of table, should be same with appendedcnt.
func TestChaos1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 12)
	schema.BlockMaxRows = 100000
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, 1)
	defer bat.Close()

	testutil.CreateRelation(t, tae, "db", schema, true)

	v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(0)
	filter := handle.NewEQFilter(v)
	var wg sync.WaitGroup
	appendCnt := uint32(0)
	deleteCnt := uint32(0)
	worker := func() {
		defer wg.Done()
		txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
		id, row, err := rel.GetByFilter(context.Background(), filter)
		// logutil.Infof("id=%v,row=%d,err=%v", id, row, err)
		if err == nil {
			err = rel.RangeDelete(id, row, row, handle.DT_Normal)
			if err != nil {
				t.Logf("delete: %v", err)
				// assert.Equal(t, txnif.ErrTxnWWConflict, err)
				assert.NoError(t, txn.Rollback(context.Background()))
				return
			}
			assert.NoError(t, txn.Commit(context.Background()))
			atomic.AddUint32(&deleteCnt, uint32(1))
			return
		}
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		err = rel.Append(context.Background(), bat)
		// TODO: enable below check later
		// assert.NotEqual(t, data.ErrDuplicate, err)
		if err == nil {
			err = txn.Commit(context.Background())
			// TODO: enable below check later
			// assert.NotEqual(t, data.ErrDuplicate, err)
			if err == nil {
				atomic.AddUint32(&appendCnt, uint32(1))
			} else {
				t.Logf("commit: %v", err)
			}
			return
		}
		_ = txn.Rollback(context.Background())
	}
	pool, _ := ants.NewPool(10)
	defer pool.Release()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		err := pool.Submit(worker)
		assert.Nil(t, err)
	}
	wg.Wait()
	t.Logf("AppendCnt: %d", appendCnt)
	t.Logf("DeleteCnt: %d", deleteCnt)
	assert.True(t, appendCnt-deleteCnt <= 1)
	_, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
	assert.Equal(t, int64(appendCnt-deleteCnt), rel.Rows())
	blk := testutil.GetOneBlock(rel)
	view, err := blk.GetColumnDataById(context.Background(), schema.GetSingleSortKeyIdx())
	assert.NoError(t, err)
	defer view.Close()
	assert.Equal(t, int(appendCnt), view.Length())
	mask := view.DeleteMask
	view.ApplyDeletes()
	t.Log(view.String())
	assert.Equal(t, int(deleteCnt), mask.GetCardinality())
}

// Testing Steps
// 1. Append 10 rows
// 2. Start txn1
// 3. Start txn2. Update the 3rd row 3rd col to int64(2222) and commit. -- PASS
// 4. Txn1 try to update the 3rd row 3rd col to int64(1111). -- W-W Conflict.
// 5. Txn1 try to delete the 3rd row. W-W Conflict. Rollback
// 6. Start txn3 and try to update th3 3rd row 3rd col to int64(3333). -- PASS
func TestSnapshotIsolation1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 12)
	schema.BlockMaxRows = 100
	bat := catalog.MockBatch(schema, 10)
	defer bat.Close()
	v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter := handle.NewEQFilter(v)

	// Step 1
	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)

	// Step 2
	txn1, rel1 := testutil.GetDefaultRelation(t, tae, schema.Name)

	// Step 3
	txn2, rel2 := testutil.GetDefaultRelation(t, tae, schema.Name)
	err := rel2.UpdateByFilter(context.Background(), filter, 3, int64(2222), false)
	assert.NoError(t, err)
	assert.NoError(t, txn2.Commit(context.Background()))

	// Step 4
	err = rel1.UpdateByFilter(context.Background(), filter, 3, int64(1111), false)
	t.Log(err)
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnWWConflict))

	// Step 5
	id, row, err := rel1.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	err = rel1.RangeDelete(id, row, row, handle.DT_Normal)
	t.Log(err)
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnWWConflict))
	_ = txn1.Rollback(context.Background())

	// Step 6
	txn3, rel3 := testutil.GetDefaultRelation(t, tae, schema.Name)
	err = rel3.UpdateByFilter(context.Background(), filter, 3, int64(3333), false)
	assert.NoError(t, err)
	assert.NoError(t, txn3.Commit(context.Background()))

	txn, rel := testutil.GetDefaultRelation(t, tae, schema.Name)
	v, _, err = rel.GetValueByFilter(context.Background(), filter, 3)
	assert.NoError(t, err)
	assert.Equal(t, int64(3333), v.(int64))
	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
}

// Testing Steps
// 1. Start txn1
// 2. Start txn2 and append one row and commit
// 3. Start txn3 and delete the row and commit
// 4. Txn1 try to append the row. (W-W). Rollback
func TestSnapshotIsolation2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.InitTestDB(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 12)
	schema.BlockMaxRows = 100
	bat := catalog.MockBatch(schema, 1)
	defer bat.Close()
	v := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(0)
	filter := handle.NewEQFilter(v)

	testutil.CreateRelation(t, tae, "db", schema, true)

	// Step 1
	txn1, rel1 := testutil.GetDefaultRelation(t, tae, schema.Name)

	// Step 2
	txn2, rel2 := testutil.GetDefaultRelation(t, tae, schema.Name)
	err := rel2.Append(context.Background(), bat)
	assert.NoError(t, err)
	assert.NoError(t, txn2.Commit(context.Background()))

	// Step 3
	txn3, rel3 := testutil.GetDefaultRelation(t, tae, schema.Name)
	err = rel3.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)
	assert.NoError(t, txn3.Commit(context.Background()))

	// Step 4
	err = rel1.Append(context.Background(), bat)
	assert.NoError(t, err)
	err = txn1.Commit(context.Background())
	t.Log(err)
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnWWConflict))
}

// 1. Append 3 blocks and delete last 5 rows of the 1st block
// 2. Merge blocks
// 3. Check rows and col[0]
func TestMergeBlocks(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	tae := testutil.InitTestDB(ctx, ModuleName, t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, -1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 3
	bat := catalog.MockBatch(schema, 30)
	defer bat.Close()

	testutil.CreateRelationAndAppend(t, 0, tae, "db", schema, bat, true)

	txn, err := tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err := txn.GetDatabase("db")
	assert.Nil(t, err)
	rel, err := db.GetRelationByName(schema.Name)
	assert.Nil(t, err)
	it := rel.MakeBlockIt()
	blkID := it.GetBlock().Fingerprint()
	err = rel.RangeDelete(blkID, 5, 9, handle.DT_Normal)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	txn, err = tae.StartTxn(nil)
	assert.Nil(t, err)
	for it.Valid() {
		testutil.CheckAllColRowsByScan(t, rel, bat.Length(), false)
		col, err := it.GetBlock().GetMeta().(*catalog.BlockEntry).GetBlockData().GetColumnDataById(context.Background(), txn, schema, 0)
		assert.NoError(t, err)
		defer col.Close()
		t.Log(col)
		it.Next()
	}
	assert.Nil(t, txn.Commit(context.Background()))

	testutil.MergeBlocks(t, 0, tae, "db", schema, false)

	txn, err = tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err = txn.GetDatabase("db")
	assert.Nil(t, err)
	rel, err = db.GetRelationByName(schema.Name)
	assert.Nil(t, err)
	assert.Equal(t, uint64(25), rel.GetMeta().(*catalog.TableEntry).GetRows())
	it = rel.MakeBlockIt()
	for it.Valid() {
		testutil.CheckAllColRowsByScan(t, rel, bat.Length()-5, false)
		col, err := it.GetBlock().GetMeta().(*catalog.BlockEntry).GetBlockData().GetColumnDataById(context.Background(), txn, schema, 0)
		assert.Nil(t, err)
		t.Log(col)
		defer col.Close()
		it.Next()
	}
	assert.Nil(t, txn.Commit(context.Background()))
}

func TestSegDelLogtail(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, -1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 3
	bat := catalog.MockBatch(schema, 30)
	defer bat.Close()

	testutil.CreateRelationAndAppend(t, 0, tae.DB, "db", schema, bat, true)

	txn, err := tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err := txn.GetDatabase("db")
	did := db.GetID()
	assert.Nil(t, err)
	rel, err := db.GetRelationByName(schema.Name)
	tid := rel.ID()
	assert.Nil(t, err)
	it := rel.MakeBlockIt()
	blkID := it.GetBlock().Fingerprint()
	err = rel.RangeDelete(blkID, 5, 9, handle.DT_Normal)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	testutil.CompactBlocks(t, 0, tae.DB, "db", schema, false)
	testutil.MergeBlocks(t, 0, tae.DB, "db", schema, false)

	t.Log(tae.Catalog.SimplePPString(common.PPL1))
	resp, close, err := logtail.HandleSyncLogTailReq(context.TODO(), new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(types.TS{}),
		CnWant: tots(types.MaxTs()),
		Table:  &api.TableID{DbId: did, TbId: tid},
	}, false)
	require.Nil(t, err)
	require.Equal(t, 3, len(resp.Commands)) // block insert + block delete + seg delete

	require.Equal(t, api.Entry_Insert, resp.Commands[0].EntryType)
	require.True(t, strings.HasSuffix(resp.Commands[0].TableName, "meta"))
	require.Equal(t, uint32(12), resp.Commands[0].Bat.Vecs[0].Len) /* 3 old blks(invalidation) + 3 old nblks (create) + 3 old nblks (invalidation) + 3 new merged nblks(create) */

	require.Equal(t, api.Entry_Delete, resp.Commands[1].EntryType)
	require.True(t, strings.HasSuffix(resp.Commands[1].TableName, "meta"))
	require.Equal(t, uint32(6), resp.Commands[1].Bat.Vecs[0].Len) /* 3 old ablks(delete) + 3 old nblks */

	require.Equal(t, api.Entry_Delete, resp.Commands[2].EntryType)
	require.True(t, strings.HasSuffix(resp.Commands[2].TableName, "seg"))
	require.Equal(t, uint32(1), resp.Commands[2].Bat.Vecs[0].Len) /* 1 old segment */

	close()

	txn, err = tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err = txn.GetDatabase("db")
	assert.Nil(t, err)
	rel, err = db.GetRelationByName(schema.Name)
	assert.Nil(t, err)
	assert.Equal(t, uint64(25), rel.GetMeta().(*catalog.TableEntry).GetRows())
	testutil.CheckAllColRowsByScan(t, rel, bat.Length()-5, false)
	assert.Nil(t, txn.Commit(context.Background()))

	err = tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.StatMaxCommitTS())
	require.NoError(t, err)

	check := func() {
		ckpEntries := tae.BGCheckpointRunner.GetAllIncrementalCheckpoints()
		require.Equal(t, 1, len(ckpEntries))
		entry := ckpEntries[0]
		ins, del, cnins, segdel, err := entry.GetByTableID(context.Background(), tae.Runtime.Fs, tid)
		require.NoError(t, err)
		require.Equal(t, uint32(6), ins.Vecs[0].Len)
		require.Equal(t, uint32(6), del.Vecs[0].Len)
		require.Equal(t, uint32(6), cnins.Vecs[0].Len)
		require.Equal(t, uint32(1), segdel.Vecs[0].Len)
		require.Equal(t, 2, len(del.Vecs))
		require.Equal(t, 2, len(segdel.Vecs))
	}
	check()

	tae.Restart(ctx)

	txn, err = tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err = txn.GetDatabase("db")
	assert.Nil(t, err)
	rel, err = db.GetRelationByName(schema.Name)
	assert.Nil(t, err)
	assert.Equal(t, uint64(25), rel.GetMeta().(*catalog.TableEntry).GetRows())
	testutil.CheckAllColRowsByScan(t, rel, bat.Length()-5, false)
	assert.Nil(t, txn.Commit(context.Background()))

	check()

}

// delete
// merge but not commit
// delete
// commit merge
func TestMergeblocks2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(1, 0)
	schema.BlockMaxRows = 3
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 6)
	bats := bat.Split(2)
	defer bat.Close()

	tae.CreateRelAndAppend(bats[0], true)

	txn, rel := tae.GetRelation()
	_ = rel.Append(context.Background(), bats[1])
	assert.Nil(t, txn.Commit(context.Background()))

	{
		v := testutil.GetSingleSortKeyValue(bat, schema, 1)
		filter := handle.NewEQFilter(v)
		txn2, rel := tae.GetRelation()
		t.Log("********before delete******************")
		testutil.CheckAllColRowsByScan(t, rel, 6, true)
		_ = rel.DeleteByFilter(context.Background(), filter)
		assert.Nil(t, txn2.Commit(context.Background()))
	}

	_, rel = tae.GetRelation()
	t.Log("**********************")
	testutil.CheckAllColRowsByScan(t, rel, 5, true)

	{
		t.Log("************merge************")

		txn, rel = tae.GetRelation()

		segIt := rel.MakeSegmentIt()
		seg := segIt.GetSegment().GetMeta().(*catalog.SegmentEntry)
		segHandle, err := rel.GetSegment(&seg.ID)
		assert.NoError(t, err)

		var metas []*catalog.BlockEntry
		it := segHandle.MakeBlockIt()
		for it.Valid() {
			meta := it.GetBlock().GetMeta().(*catalog.BlockEntry)
			metas = append(metas, meta)
			it.Next()
		}
		segsToMerge := []*catalog.SegmentEntry{segHandle.GetMeta().(*catalog.SegmentEntry)}
		task, err := jobs.NewMergeBlocksTask(nil, txn, metas, segsToMerge, nil, tae.Runtime)
		assert.NoError(t, err)
		err = task.OnExec(context.Background())
		assert.NoError(t, err)

		{
			v := testutil.GetSingleSortKeyValue(bat, schema, 2)
			filter := handle.NewEQFilter(v)
			txn2, rel := tae.GetRelation()
			t.Log("********before delete******************")
			testutil.CheckAllColRowsByScan(t, rel, 5, true)
			_ = rel.DeleteByFilter(context.Background(), filter)
			assert.Nil(t, txn2.Commit(context.Background()))
		}
		err = txn.Commit(context.Background())
		assert.NoError(t, err)
	}

	t.Log("********************")
	_, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 4, true)
	assert.Equal(t, int64(4), rel.Rows())

	v := testutil.GetSingleSortKeyValue(bat, schema, 1)
	filter := handle.NewEQFilter(v)
	_, _, err := rel.GetByFilter(context.Background(), filter)
	assert.NotNil(t, err)

	v = testutil.GetSingleSortKeyValue(bat, schema, 2)
	filter = handle.NewEQFilter(v)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.NotNil(t, err)

	// v = testutil.GetSingleSortKeyValue(bat, schema, 4)
	// filter = handle.NewEQFilter(v)
	// _, _, err = rel.GetByFilter(context.Background(),filter)
	// assert.NotNil(t, err)

	// tae.Restart()
	// assert.Equal(t, int64(2), rel.Rows())
}
func TestMergeEmptyBlocks(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(1, 0)
	schema.BlockMaxRows = 3
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 6)
	bats := bat.Split(2)
	defer bat.Close()

	tae.CreateRelAndAppend(bats[0], true)

	assert.NoError(t, tae.DeleteAll(true))

	txn, rel := tae.GetRelation()
	assert.NoError(t, rel.Append(context.Background(), bats[1]))
	assert.NoError(t, txn.Commit(context.Background()))

	{
		t.Log("************merge************")

		txn, rel = tae.GetRelation()

		segIt := rel.MakeSegmentIt()
		seg := segIt.GetSegment().GetMeta().(*catalog.SegmentEntry)
		segHandle, err := rel.GetSegment(&seg.ID)
		assert.NoError(t, err)

		var metas []*catalog.BlockEntry
		it := segHandle.MakeBlockIt()
		for it.Valid() {
			meta := it.GetBlock().GetMeta().(*catalog.BlockEntry)
			metas = append(metas, meta)
			it.Next()
		}
		segsToMerge := []*catalog.SegmentEntry{segHandle.GetMeta().(*catalog.SegmentEntry)}
		task, err := jobs.NewMergeBlocksTask(nil, txn, metas, segsToMerge, nil, tae.Runtime)
		assert.NoError(t, err)
		err = task.OnExec(context.Background())
		assert.NoError(t, err)

		{
			v := testutil.GetSingleSortKeyValue(bat, schema, 4)
			filter := handle.NewEQFilter(v)
			txn2, rel := tae.GetRelation()
			_ = rel.DeleteByFilter(context.Background(), filter)
			assert.Nil(t, txn2.Commit(context.Background()))
		}
		err = txn.Commit(context.Background())
		assert.NoError(t, err)
	}
}
func TestDelete2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(18, 11)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 5)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)

	txn, rel := tae.GetRelation()
	v := testutil.GetSingleSortKeyValue(bat, schema, 2)
	filter := handle.NewEQFilter(v)
	err := rel.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.CompactBlocks(false)
}

func TestNull1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(18, 9)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)

	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*3+1))
	defer bat.Close()
	bats := bat.Split(4)
	bats[0].Vecs[3].Update(2, nil, true)
	tae.CreateRelAndAppend(bats[0], true)

	txn, rel := tae.GetRelation()
	blk := testutil.GetOneBlock(rel)
	view, err := blk.GetColumnDataById(context.Background(), 3)
	assert.NoError(t, err)
	defer view.Close()
	//v := view.GetData().Get(2)
	assert.True(t, view.GetData().IsNull(2))
	testutil.CheckAllColRowsByScan(t, rel, bats[0].Length(), false)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.Restart(ctx)
	txn, rel = tae.GetRelation()
	blk = testutil.GetOneBlock(rel)
	view, err = blk.GetColumnDataById(context.Background(), 3)
	assert.NoError(t, err)
	defer view.Close()
	//v = view.GetData().Get(2)
	assert.True(t, view.GetData().IsNull(2))
	testutil.CheckAllColRowsByScan(t, rel, bats[0].Length(), false)

	v := testutil.GetSingleSortKeyValue(bats[0], schema, 2)
	filter_2 := handle.NewEQFilter(v)
	_, uv0_2_isNull, err := rel.GetValueByFilter(context.Background(), filter_2, 3)
	assert.NoError(t, err)
	assert.True(t, uv0_2_isNull)

	v0_4 := testutil.GetSingleSortKeyValue(bats[0], schema, 4)
	filter_4 := handle.NewEQFilter(v0_4)
	err = rel.UpdateByFilter(context.Background(), filter_4, 3, nil, true)
	assert.NoError(t, err)
	_, uv_isNull, err := rel.GetValueByFilter(context.Background(), filter_4, 3)
	assert.NoError(t, err)
	assert.True(t, uv_isNull)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, bats[0].Length(), true)
	_, uv_isNull, err = rel.GetValueByFilter(context.Background(), filter_4, 3)
	assert.NoError(t, err)
	assert.True(t, uv_isNull)

	err = rel.Append(context.Background(), bats[1])
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.CompactBlocks(false)
	txn, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, testutil.LenOfBats(bats[:2]), false)
	_, uv_isNull, err = rel.GetValueByFilter(context.Background(), filter_4, 3)
	assert.NoError(t, err)
	assert.True(t, uv_isNull)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.Restart(ctx)
	txn, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, testutil.LenOfBats(bats[:2]), false)
	_, uv_isNull, err = rel.GetValueByFilter(context.Background(), filter_4, 3)
	assert.NoError(t, err)
	assert.True(t, uv_isNull)

	v0_1 := testutil.GetSingleSortKeyValue(bats[0], schema, 1)
	filter0_1 := handle.NewEQFilter(v0_1)
	err = rel.UpdateByFilter(context.Background(), filter0_1, 12, nil, true)
	assert.NoError(t, err)
	_, uv0_1_isNull, err := rel.GetValueByFilter(context.Background(), filter0_1, 12)
	assert.NoError(t, err)
	assert.True(t, uv0_1_isNull)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	_, uv0_1_isNull, err = rel.GetValueByFilter(context.Background(), filter0_1, 12)
	assert.NoError(t, err)
	assert.True(t, uv0_1_isNull)
	err = rel.Append(context.Background(), bats[2])
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.CompactBlocks(false)
	tae.MergeBlocks(false)

	txn, rel = tae.GetRelation()
	_, uv0_1_isNull, err = rel.GetValueByFilter(context.Background(), filter0_1, 12)
	assert.NoError(t, err)
	assert.True(t, uv0_1_isNull)
	_, uv0_2_isNull, err = rel.GetValueByFilter(context.Background(), filter_2, 3)
	assert.NoError(t, err)
	assert.True(t, uv0_2_isNull)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.Restart(ctx)

	txn, rel = tae.GetRelation()
	_, uv0_1_isNull, err = rel.GetValueByFilter(context.Background(), filter0_1, 12)
	assert.NoError(t, err)
	assert.True(t, uv0_1_isNull)
	_, uv0_2_isNull, err = rel.GetValueByFilter(context.Background(), filter_2, 3)
	assert.NoError(t, err)
	assert.True(t, uv0_2_isNull)
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestTruncate(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(18, 15)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*5+1))
	defer bat.Close()
	bats := bat.Split(20)
	tae.CreateRelAndAppend(bats[0], true)

	var wg sync.WaitGroup
	p, _ := ants.NewPool(10)
	defer p.Release()
	tryAppend := func(i int) func() {
		return func() {
			defer wg.Done()
			tae.TryAppend(bats[1+i])
		}
	}

	for i := range bats[1:] {
		if i == 10 {
			wg.Add(1)
			_ = p.Submit(func() {
				defer wg.Done()
				tae.Truncate()
				t.Log(tae.Catalog.SimplePPString(common.PPL1))
			})
		}
		wg.Add(1)
		_ = p.Submit(tryAppend(i))
		time.Sleep(time.Millisecond * 2)
	}
	wg.Wait()
	txn, rel := tae.GetRelation()
	t.Logf("Rows: %d", rel.Rows())
	assert.NoError(t, txn.Commit(context.Background()))
	tae.Truncate()
	txn, rel = tae.GetRelation()
	assert.Zero(t, 0, rel.Rows())
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestGetColumnData(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(18, 13)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 39)
	bats := bat.Split(4)
	defer bat.Close()
	tae.CreateRelAndAppend(bats[0], true)
	txn, rel := tae.GetRelation()
	blk := testutil.GetOneBlock(rel)
	view, _ := blk.GetColumnDataById(context.Background(), 2)
	defer view.Close()
	assert.Equal(t, bats[0].Length(), view.Length())
	assert.NotZero(t, view.GetData().Allocated())

	view, _ = blk.GetColumnDataById(context.Background(), 2)
	defer view.Close()
	assert.Equal(t, bats[0].Length(), view.Length())
	assert.NotZero(t, view.GetData().Allocated())
	assert.NoError(t, txn.Commit(context.Background()))

	tae.CompactBlocks(false)
	txn, rel = tae.GetRelation()
	blk = testutil.GetOneBlock(rel)
	view, _ = blk.GetColumnDataById(context.Background(), 2)
	defer view.Close()
	assert.Equal(t, bats[0].Length(), view.Length())
	assert.NotZero(t, view.GetData().Allocated())

	view, _ = blk.GetColumnDataById(context.Background(), 2)
	defer view.Close()
	assert.Equal(t, bats[0].Length(), view.Length())
	assert.NotZero(t, view.GetData().Allocated())
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	err := rel.Append(context.Background(), bats[1])
	assert.NoError(t, err)
	blk = testutil.GetOneBlock(rel)
	view, err = blk.GetColumnDataById(context.Background(), 2)
	assert.NoError(t, err)
	defer view.Close()
	assert.True(t, view.GetData().Equals(bats[1].Vecs[2]))
	assert.NotZero(t, view.GetData().Allocated())
	view, err = blk.GetColumnDataById(context.Background(), 2)
	assert.NoError(t, err)
	defer view.Close()
	assert.True(t, view.GetData().Equals(bats[1].Vecs[2]))
	assert.NotZero(t, view.GetData().Allocated())

	assert.NoError(t, txn.Commit(context.Background()))
}

func TestCompactBlk1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 5)
	bats := bat.Split(5)
	defer bat.Close()

	tae.CreateRelAndAppend(bats[2], true)

	txn, rel := tae.GetRelation()
	_ = rel.Append(context.Background(), bats[1])
	assert.Nil(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[3])
	assert.Nil(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[4])
	assert.Nil(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[0])
	assert.Nil(t, txn.Commit(context.Background()))

	{
		v := testutil.GetSingleSortKeyValue(bat, schema, 1)
		t.Logf("v is %v**********", v)
		filter := handle.NewEQFilter(v)
		txn2, rel := tae.GetRelation()
		t.Log("********before delete******************")
		testutil.CheckAllColRowsByScan(t, rel, 5, true)
		_ = rel.DeleteByFilter(context.Background(), filter)
		assert.Nil(t, txn2.Commit(context.Background()))
	}

	_, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 4, true)

	{
		t.Log("************compact************")
		txn, rel = tae.GetRelation()
		it := rel.MakeBlockIt()
		blk := it.GetBlock()
		meta := blk.GetMeta().(*catalog.BlockEntry)
		task, err := jobs.NewCompactBlockTask(nil, txn, meta, tae.DB.Runtime)
		assert.NoError(t, err)
		err = task.OnExec(context.Background())
		assert.NoError(t, err)

		{
			v := testutil.GetSingleSortKeyValue(bat, schema, 2)
			t.Logf("v is %v**********", v)
			filter := handle.NewEQFilter(v)
			txn2, rel := tae.GetRelation()
			t.Log("********before delete******************")
			testutil.CheckAllColRowsByScan(t, rel, 4, true)
			_ = rel.DeleteByFilter(context.Background(), filter)
			assert.Nil(t, txn2.Commit(context.Background()))
		}

		err = txn.Commit(context.Background())
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnWWConflict))
	}

	_, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 3, true)
	assert.Equal(t, int64(3), rel.Rows())

	tae.Restart(ctx)
	_, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 3, true)
	assert.Equal(t, int64(3), rel.Rows())
}

func TestCompactBlk2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 5)
	bats := bat.Split(5)
	defer bat.Close()

	tae.CreateRelAndAppend(bats[2], true)

	txn, rel := tae.GetRelation()
	_ = rel.Append(context.Background(), bats[1])
	assert.Nil(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[3])
	assert.Nil(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[4])
	assert.Nil(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[0])
	assert.Nil(t, txn.Commit(context.Background()))

	v := testutil.GetSingleSortKeyValue(bat, schema, 1)
	t.Logf("v is %v**********", v)
	filter := handle.NewEQFilter(v)
	txn2, rel1 := tae.GetRelation()
	t.Log("********before delete******************")
	testutil.CheckAllColRowsByScan(t, rel1, 5, true)
	_ = rel1.DeleteByFilter(context.Background(), filter)
	assert.Nil(t, txn2.Commit(context.Background()))

	_, rel2 := tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel2, 4, true)

	t.Log("************compact************")
	txn, rel = tae.GetRelation()
	it := rel.MakeBlockIt()
	blk := it.GetBlock()
	meta := blk.GetMeta().(*catalog.BlockEntry)
	task, err := jobs.NewCompactBlockTask(nil, txn, meta, tae.DB.Runtime)
	assert.NoError(t, err)
	err = task.OnExec(context.Background())
	assert.NoError(t, err)
	err = txn.Commit(context.Background())
	assert.NoError(t, err)

	v = testutil.GetSingleSortKeyValue(bat, schema, 2)
	t.Logf("v is %v**********", v)
	filter = handle.NewEQFilter(v)
	txn2, rel3 := tae.GetRelation()
	t.Log("********before delete******************")
	testutil.CheckAllColRowsByScan(t, rel3, 4, true)
	_ = rel3.DeleteByFilter(context.Background(), filter)
	assert.Nil(t, txn2.Commit(context.Background()))

	v = testutil.GetSingleSortKeyValue(bat, schema, 4)
	t.Logf("v is %v**********", v)
	filter = handle.NewEQFilter(v)
	txn2, rel4 := tae.GetRelation()
	t.Log("********before delete******************")
	testutil.CheckAllColRowsByScan(t, rel4, 3, true)
	_ = rel4.DeleteByFilter(context.Background(), filter)
	assert.Nil(t, txn2.Commit(context.Background()))

	testutil.CheckAllColRowsByScan(t, rel1, 5, true)
	testutil.CheckAllColRowsByScan(t, rel2, 4, true)

	_, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 2, true)
	assert.Equal(t, int64(2), rel.Rows())

	v = testutil.GetSingleSortKeyValue(bat, schema, 2)
	filter = handle.NewEQFilter(v)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.NotNil(t, err)

	v = testutil.GetSingleSortKeyValue(bat, schema, 4)
	filter = handle.NewEQFilter(v)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.NotNil(t, err)

	tae.Restart(ctx)
	assert.Equal(t, int64(2), rel.Rows())
}

func TestCompactblk3(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 3)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)

	v := testutil.GetSingleSortKeyValue(bat, schema, 1)
	filter := handle.NewEQFilter(v)
	txn2, rel1 := tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel1, 3, true)
	_ = rel1.DeleteByFilter(context.Background(), filter)
	assert.Nil(t, txn2.Commit(context.Background()))

	_, rel2 := tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel2, 2, true)

	txn, rel := tae.GetRelation()
	it := rel.MakeBlockIt()
	blk := it.GetBlock()
	meta := blk.GetMeta().(*catalog.BlockEntry)
	task, err := jobs.NewCompactBlockTask(nil, txn, meta, tae.DB.Runtime)
	assert.NoError(t, err)
	err = task.OnExec(context.Background())
	assert.NoError(t, err)
	err = txn.Commit(context.Background())
	assert.NoError(t, err)

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	processor := &catalog.LoopProcessor{}
	processor.BlockFn = func(be *catalog.BlockEntry) error {
		if be.GetSegment().GetTable().GetDB().IsSystemDB() {
			return nil
		}
		view, err := be.GetBlockData().GetColumnDataById(context.Background(), txn, schema, 0)
		assert.NoError(t, err)
		view.ApplyDeletes()
		assert.Equal(t, 2, view.Length())
		return nil
	}
	err = tae.Catalog.RecurLoop(processor)
	assert.NoError(t, err)
}

func TestImmutableIndexInAblk(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 5)
	bats := bat.Split(5)
	defer bat.Close()

	tae.CreateRelAndAppend(bats[2], true)
	txn, rel := tae.GetRelation()
	_ = rel.Append(context.Background(), bats[1])
	assert.Nil(t, txn.Commit(context.Background()))
	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[3])
	assert.Nil(t, txn.Commit(context.Background()))
	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[4])
	assert.Nil(t, txn.Commit(context.Background()))
	txn, rel = tae.GetRelation()
	_ = rel.Append(context.Background(), bats[0])
	assert.Nil(t, txn.Commit(context.Background()))

	v := testutil.GetSingleSortKeyValue(bat, schema, 1)
	filter := handle.NewEQFilter(v)
	txn2, rel := tae.GetRelation()
	_ = rel.DeleteByFilter(context.Background(), filter)
	assert.Nil(t, txn2.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	it := rel.MakeBlockIt()
	blk := it.GetBlock()
	meta := blk.GetMeta().(*catalog.BlockEntry)
	task, err := jobs.NewCompactBlockTask(nil, txn, meta, tae.DB.Runtime)
	assert.NoError(t, err)
	err = task.OnExec(context.Background())
	assert.NoError(t, err)
	err = txn.Commit(context.Background())
	assert.NoError(t, err)

	txn, _ = tae.GetRelation()
	_, err = meta.GetBlockData().GetByFilter(context.Background(), txn, filter)
	assert.Error(t, err)
	v = testutil.GetSingleSortKeyValue(bat, schema, 2)
	filter = handle.NewEQFilter(v)
	_, err = meta.GetBlockData().GetByFilter(context.Background(), txn, filter)
	assert.NoError(t, err)

	err = meta.GetBlockData().BatchDedup(context.Background(), txn, bat.Vecs[1], nil, nil, false, objectio.BloomFilter{})
	assert.Error(t, err)
}

func TestDelete3(t *testing.T) {
	// t.Skip(any("This case crashes occasionally, is being fixed, skip it for now"))
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	// this task won't affect logic of TestAppend2, it just prints logs about dirty count
	forest := logtail.NewDirtyCollector(tae.LogtailMgr, opts.Clock, tae.Catalog, new(catalog.LoopProcessor))
	hb := ops.NewHeartBeaterWithFunc(5*time.Millisecond, func() {
		forest.Run()
		t.Log(forest.String())
	}, nil)
	hb.Start()
	defer hb.Stop()
	schema := catalog.MockSchemaAll(3, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	// rows := int(schema.BlockMaxRows * 1)
	rows := int(schema.BlockMaxRows*3) + 1
	bat := catalog.MockBatch(schema, rows)

	tae.CreateRelAndAppend(bat, true)
	tae.CheckRowsByScan(rows, false)
	deleted := false
	for i := 0; i < 10; i++ {
		if deleted {
			tae.CheckRowsByScan(0, true)
			tae.DoAppend(bat)
			deleted = false
			tae.CheckRowsByScan(rows, true)
		} else {
			tae.CheckRowsByScan(rows, true)
			err := tae.DeleteAll(true)
			if err == nil {
				deleted = true
				tae.CheckRowsByScan(0, true)
				// assert.Zero(t, tae.getRows())
			} else {
				tae.CheckRowsByScan(rows, true)
				// assert.Equal(t, tae.getRows(), rows)
			}
		}
	}
	t.Logf(tae.Catalog.SimplePPString(common.PPL1))
}

func TestDropCreated1(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	txn, err := tae.StartTxn(nil)
	assert.Nil(t, err)
	_, err = txn.CreateDatabase("db", "", "")
	assert.Nil(t, err)
	db, err := txn.DropDatabase("db")
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	assert.Equal(t, txn.GetCommitTS(), db.GetMeta().(*catalog.DBEntry).GetCreatedAt())
	assert.Equal(t, txn.GetCommitTS(), db.GetMeta().(*catalog.DBEntry).GetCreatedAt())

	tae.Restart(ctx)
}

func TestDropCreated2(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	schema := catalog.MockSchemaAll(1, -1)
	defer tae.Close()

	txn, err := tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err := txn.CreateDatabase("db", "", "")
	assert.Nil(t, err)
	rel, err := db.CreateRelation(schema)
	assert.Nil(t, err)
	_, err = db.DropRelationByName(schema.Name)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	assert.Equal(t, txn.GetCommitTS(), rel.GetMeta().(*catalog.TableEntry).GetCreatedAt())
	assert.Equal(t, txn.GetCommitTS(), rel.GetMeta().(*catalog.TableEntry).GetCreatedAt())

	tae.Restart(ctx)
}

func TestDropCreated3(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	txn, err := tae.StartTxn(nil)
	assert.Nil(t, err)
	_, err = txn.CreateDatabase("db", "", "")
	assert.Nil(t, err)
	_, err = txn.DropDatabase("db")
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	err = tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.Now())
	assert.Nil(t, err)

	tae.Restart(ctx)
}

func TestDropCreated4(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	schema := catalog.MockSchemaAll(1, -1)
	defer tae.Close()

	txn, err := tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err := txn.CreateDatabase("db", "", "")
	assert.Nil(t, err)
	_, err = db.CreateRelation(schema)
	assert.Nil(t, err)
	_, err = db.DropRelationByName(schema.Name)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	err = tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.Now())
	assert.Nil(t, err)

	tae.Restart(ctx)
}

// records create at 1 and commit
// read by ts 1, err should be nil
func TestReadEqualTS(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	txn, err := tae.StartTxn(nil)
	tae.Catalog.Lock()
	tae.Catalog.CreateDBEntryByTS("db", txn.GetStartTS())
	tae.Catalog.Unlock()
	assert.Nil(t, err)
	_, err = txn.GetDatabase("db")
	assert.Nil(t, err)
}

func TestTruncateZonemap(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	type Mod struct {
		offset int
		v      byte
	}
	mockBytes := func(init byte, size int, mods ...Mod) []byte {
		ret := make([]byte, size)
		for i := 0; i < size; i++ {
			ret[i] = init
		}
		for _, m := range mods {
			ret[m.offset] = m.v
		}
		return ret
	}
	testutils.EnsureNoLeak(t)
	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(13, 12) // set varchar PK
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)

	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*2+9))        // 2.9 blocks
	minv := mockBytes(0, 35)                                              // 0x00000000
	trickyMinv := mockBytes(0, 33)                                        // smaller than minv, not in mut index but in immut index
	maxv := mockBytes(0xff, 35, Mod{0, 0x61}, Mod{1, 0x62}, Mod{2, 0x63}) // abc0xff0xff...
	trickyMaxv := []byte("abd")                                           // bigger than maxv, not in mut index but in immut index
	bat.Vecs[12].Update(8, maxv, false)
	bat.Vecs[12].Update(11, minv, false)
	bat.Vecs[12].Update(22, []byte("abcc"), false)
	defer bat.Close()

	checkMinMax := func(rel handle.Relation, minvOffset, maxvOffset uint32) {
		_, _, err := rel.GetByFilter(context.Background(), handle.NewEQFilter(trickyMinv))
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		_, _, err = rel.GetByFilter(context.Background(), handle.NewEQFilter(trickyMaxv))
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		_, row, err := rel.GetByFilter(context.Background(), handle.NewEQFilter(minv))
		assert.NoError(t, err)
		assert.Equal(t, minvOffset, row)
		_, row, err = rel.GetByFilter(context.Background(), handle.NewEQFilter(maxv))
		assert.NoError(t, err)
		assert.Equal(t, maxvOffset, row)
	}

	tae.CreateRelAndAppend(bat, true)

	// runtime check
	txn, rel := tae.GetRelation()
	checkMinMax(rel, 1, 8)
	assert.NoError(t, txn.Commit(context.Background()))

	// restart without compact
	tae.Restart(ctx)
	txn, rel = tae.GetRelation()
	checkMinMax(rel, 1, 8)
	assert.NoError(t, txn.Commit(context.Background()))

	// restart with compact
	tae.CompactBlocks(false)
	tae.MergeBlocks(false)
	tae.Restart(ctx)
	txn, rel = tae.GetRelation()
	checkMinMax(rel, 0, 9)
	assert.NoError(t, txn.Commit(context.Background()))

	// 3 NonAppendable Blocks
	txn, rel = tae.GetRelation()
	rel.UpdateByFilter(context.Background(), handle.NewEQFilter(maxv), 12, mockBytes(0xff, 35), false)
	assert.NoError(t, txn.Commit(context.Background()))
	tae.CompactBlocks(false)
	tae.MergeBlocks(false)
	tae.Restart(ctx)

	txn, rel = tae.GetRelation()
	_, row, err := rel.GetByFilter(context.Background(), handle.NewEQFilter(mockBytes(0xff, 35)))
	assert.NoError(t, err)
	assert.Equal(t, uint32(9), row)
	assert.NoError(t, txn.Commit(context.Background()))
}

func mustStartTxn(t *testing.T, tae *testutil.TestEngine, tenantID uint32) txnif.AsyncTxn {
	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	txn.BindAccessInfo(tenantID, 0, 0)
	return txn
}

func TestMultiTenantDBOps(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	var err error
	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	txn11 := mustStartTxn(t, tae, 1)
	_, err = txn11.CreateDatabase("db", "", "")
	assert.NoError(t, err)
	txn12 := mustStartTxn(t, tae, 1)
	_, err = txn11.CreateDatabase("db", "", "")
	assert.Error(t, err)

	txn21 := mustStartTxn(t, tae, 2)
	_, err = txn21.CreateDatabase("db", "", "")
	assert.NoError(t, err)

	assert.NoError(t, txn11.Commit(context.Background()))
	assert.NoError(t, txn12.Commit(context.Background()))
	assert.NoError(t, txn21.Commit(context.Background()))

	txn22 := mustStartTxn(t, tae, 2)
	_, _ = txn22.CreateDatabase("db2", "", "")

	txn23 := mustStartTxn(t, tae, 2)
	// [mo_catalog, db]
	assert.Equal(t, 2, len(txn23.DatabaseNames()))
	assert.NoError(t, txn23.Commit(context.Background()))

	txn22.Commit(context.Background())
	tae.Restart(ctx)

	txn24 := mustStartTxn(t, tae, 2)
	// [mo_catalog, db, db2]
	assert.Equal(t, 3, len(txn24.DatabaseNames()))
	assert.NoError(t, txn24.Commit(context.Background()))

	txn13 := mustStartTxn(t, tae, 1)
	// [mo_catalog, db]
	assert.Equal(t, 2, len(txn13.DatabaseNames()))

	_, err = txn13.GetDatabase("db2")
	assert.Error(t, err)
	dbHdl, err := txn13.GetDatabase("db")
	assert.NoError(t, err)
	assert.Equal(t, uint32(1), dbHdl.GetMeta().(*catalog.DBEntry).GetTenantID())

	_, err = txn13.DropDatabase("db2")
	assert.Error(t, err)
	_, err = txn13.DropDatabase("db")
	assert.NoError(t, err)
	assert.NoError(t, txn13.Commit(context.Background()))

	txn14 := mustStartTxn(t, tae, 1)
	// [mo_catalog]
	assert.Equal(t, 1, len(txn14.DatabaseNames()))
	assert.NoError(t, txn14.Commit(context.Background()))
}

func TestMultiTenantMoCatalogOps(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	var err error
	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	s := catalog.MockSchemaAll(1, 0)
	s.Name = "mo_accounts"
	txn0, sysDB := tae.GetDB(pkgcatalog.MO_CATALOG)
	_, err = sysDB.CreateRelation(s)
	assert.NoError(t, err)
	assert.NoError(t, txn0.Commit(context.Background()))

	schema11 := catalog.MockSchemaAll(3, 0)
	schema11.BlockMaxRows = 10
	schema11.SegmentMaxBlocks = 2
	tae.BindSchema(schema11)
	tae.BindTenantID(1)

	bat1 := catalog.MockBatch(schema11, int(schema11.BlockMaxRows*2+9))
	tae.CreateRelAndAppend(bat1, true)
	// pretend 'mo_users'
	s = catalog.MockSchemaAll(1, 0)
	s.Name = "mo_users"
	txn11, sysDB := tae.GetDB(pkgcatalog.MO_CATALOG)
	_, err = sysDB.CreateRelation(s)
	assert.NoError(t, err)
	assert.NoError(t, txn11.Commit(context.Background()))

	tae.CompactBlocks(false)
	tae.MergeBlocks(false)

	schema21 := catalog.MockSchemaAll(2, 1)
	schema21.BlockMaxRows = 10
	schema21.SegmentMaxBlocks = 2
	tae.BindSchema(schema21)
	tae.BindTenantID(2)

	bat2 := catalog.MockBatch(schema21, int(schema21.BlockMaxRows*3+5))
	tae.CreateRelAndAppend(bat2, true)
	txn21, sysDB := tae.GetDB(pkgcatalog.MO_CATALOG)
	s = catalog.MockSchemaAll(1, 0)
	s.Name = "mo_users"
	_, err = sysDB.CreateRelation(s)
	assert.NoError(t, err)
	assert.NoError(t, txn21.Commit(context.Background()))

	tae.CompactBlocks(false)
	tae.MergeBlocks(false)

	tae.Restart(ctx)

	reservedColumnsCnt := len(catalog.SystemDBSchema.ColDefs) +
		len(catalog.SystemColumnSchema.ColDefs) +
		len(catalog.SystemTableSchema.ColDefs)
	{
		// account 2
		// check data for good
		_, tbl := tae.GetRelation()
		testutil.CheckAllColRowsByScan(t, tbl, 35, false)
		// [mo_catalog, db]
		assert.Equal(t, 2, len(mustStartTxn(t, tae, 2).DatabaseNames()))
		_, sysDB = tae.GetDB(pkgcatalog.MO_CATALOG)
		sysDB.Relations()
		sysDBTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_DATABASE)
		// [mo_catalog, db]
		testutil.CheckAllColRowsByScan(t, sysDBTbl, 2, true)
		sysTblTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_TABLES)
		// [mo_database, mo_tables, mo_columns, 'mo_users_t2' 'test-table-a-timestamp']
		testutil.CheckAllColRowsByScan(t, sysTblTbl, 5, true)
		sysColTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_COLUMNS)
		// [mo_database(8), mo_tables(13), mo_columns(19), 'mo_users_t2'(1+1), 'test-table-a-timestamp'(2+1)]
		testutil.CheckAllColRowsByScan(t, sysColTbl, reservedColumnsCnt+5, true)
	}
	{
		// account 1
		tae.BindSchema(schema11)
		tae.BindTenantID(1)
		// check data for good
		_, tbl := tae.GetRelation()
		testutil.CheckAllColRowsByScan(t, tbl, 29, false)
		// [mo_catalog, db]
		assert.Equal(t, 2, len(mustStartTxn(t, tae, 1).DatabaseNames()))
		_, sysDB = tae.GetDB(pkgcatalog.MO_CATALOG)
		sysDB.Relations()
		sysDBTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_DATABASE)
		// [mo_catalog, db]
		testutil.CheckAllColRowsByScan(t, sysDBTbl, 2, true)
		sysTblTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_TABLES)
		// [mo_database, mo_tables, mo_columns, 'mo_users_t1' 'test-table-a-timestamp']
		testutil.CheckAllColRowsByScan(t, sysTblTbl, 5, true)
		sysColTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_COLUMNS)
		// [mo_database(8), mo_tables(13), mo_columns(19), 'mo_users_t1'(1+1), 'test-table-a-timestamp'(3+1)]
		testutil.CheckAllColRowsByScan(t, sysColTbl, reservedColumnsCnt+6, true)
	}
	{
		// sys account
		tae.BindSchema(nil)
		tae.BindTenantID(0)
		// [mo_catalog]
		assert.Equal(t, 1, len(mustStartTxn(t, tae, 0).DatabaseNames()))
		_, sysDB = tae.GetDB(pkgcatalog.MO_CATALOG)
		sysDB.Relations()
		sysDBTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_DATABASE)
		// [mo_catalog]
		testutil.CheckAllColRowsByScan(t, sysDBTbl, 1, true)
		sysTblTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_TABLES)
		// [mo_database, mo_tables, mo_columns, 'mo_accounts']
		testutil.CheckAllColRowsByScan(t, sysTblTbl, 4, true)
		sysColTbl, _ := sysDB.GetRelationByName(pkgcatalog.MO_COLUMNS)
		// [mo_database(8), mo_tables(13), mo_columns(19), 'mo_accounts'(1+1)]
		testutil.CheckAllColRowsByScan(t, sysColTbl, reservedColumnsCnt+2, true)
	}

}

// txn1 create update
// txn2 update delete
func TestUpdateAttr(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	schema := catalog.MockSchemaAll(1, -1)
	defer tae.Close()

	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	db, err := txn.CreateDatabase("db", "", "")
	assert.NoError(t, err)
	rel, err := db.CreateRelation(schema)
	assert.NoError(t, err)
	seg, err := rel.CreateSegment(false)
	assert.NoError(t, err)
	blk, err := seg.CreateBlock(false)
	assert.NoError(t, err)
	blk.GetMeta().(*catalog.BlockEntry).UpdateMetaLoc(txn, objectio.Location("test_1"))
	assert.NoError(t, txn.Commit(context.Background()))

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	db, err = txn.GetDatabase("db")
	assert.NoError(t, err)
	rel, err = db.GetRelationByName(schema.Name)
	assert.NoError(t, err)
	seg, err = rel.GetSegment(seg.GetID())
	assert.NoError(t, err)
	blk, err = seg.CreateBlock(false)
	assert.NoError(t, err)
	blk.GetMeta().(*catalog.BlockEntry).UpdateDeltaLoc(txn, objectio.Location("test_2"))
	rel.SoftDeleteSegment(seg.GetID())
	assert.NoError(t, txn.Commit(context.Background()))

	t.Log(tae.Catalog.SimplePPString(3))

	tae.Restart(ctx)

	t.Log(tae.Catalog.SimplePPString(3))
}

type dummyCpkGetter struct{}

func (c *dummyCpkGetter) CollectCheckpointsInRange(ctx context.Context, start, end types.TS) (ckpLoc string, lastEnd types.TS, err error) {
	return "", types.TS{}, nil
}

func (c *dummyCpkGetter) FlushTable(ctx context.Context, dbID, tableID uint64, ts types.TS) error {
	return nil
}

func tots(ts types.TS) *timestamp.Timestamp {
	t := ts.ToTimestamp()
	return &t
}

func TestLogtailBasic(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	opts.LogtailCfg = &options.LogtailCfg{PageSize: 30}
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	logMgr := tae.LogtailMgr
	defer tae.Close()

	// at first, we can see nothing
	minTs, maxTs := types.BuildTS(0, 0), types.BuildTS(1000, 1000)
	reader := logMgr.GetReader(minTs, maxTs)
	require.False(t, reader.HasCatalogChanges())
	require.Equal(t, 0, len(reader.GetDirtyByTable(1000, 1000).Segs))

	schema := catalog.MockSchemaAll(2, -1)
	schema.Name = "test"
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	// craete 2 db and 2 tables
	txn, _ := tae.StartTxn(nil)
	todropdb, _ := txn.CreateDatabase("todrop", "", "")
	todropdb.CreateRelation(schema)
	db, _ := txn.CreateDatabase("db", "", "")
	tbl, _ := db.CreateRelation(schema)
	dbID := db.GetID()
	tableID := tbl.ID()
	txn.Commit(context.Background())
	catalogWriteTs := txn.GetPrepareTS()

	// drop the first db
	txn2, _ := tae.StartTxn(nil)
	txn2.DropDatabase("todrop")
	txn2.Commit(context.Background())
	catalogDropTs := txn2.GetPrepareTS()

	writeTs := make([]types.TS, 0, 120)
	deleteRowIDs := make([]types.Rowid, 0, 10)

	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		// insert 100 rows
		for i := 0; i < 100; i++ {
			txn, _ := tae.StartTxn(nil)
			db, _ := txn.GetDatabase("db")
			tbl, _ := db.GetRelationByName("test")
			tbl.Append(context.Background(), catalog.MockBatch(schema, 1))
			require.NoError(t, txn.Commit(context.Background()))
			writeTs = append(writeTs, txn.GetPrepareTS())
		}
		// delete the row whose offset is 5 for every block
		{
			// collect rowid
			txn, _ := tae.StartTxn(nil)
			db, _ := txn.GetDatabase("db")
			tbl, _ := db.GetRelationByName("test")
			blkIt := tbl.MakeBlockIt()
			for ; blkIt.Valid(); blkIt.Next() {
				id := blkIt.GetBlock().GetMeta().(*catalog.BlockEntry).ID
				deleteRowIDs = append(deleteRowIDs, *objectio.NewRowid(&id, 5))
			}
			require.NoError(t, txn.Commit(context.Background()))
		}

		// delete two 2 rows one time. no special reason, it just comes up
		for i := 0; i < len(deleteRowIDs); i += 2 {
			txn, _ := tae.StartTxn(nil)
			db, _ := txn.GetDatabase("db")
			tbl, _ := db.GetRelationByName("test")
			require.NoError(t, tbl.DeleteByPhyAddrKey(deleteRowIDs[i]))
			if i+1 < len(deleteRowIDs) {
				tbl.DeleteByPhyAddrKey(deleteRowIDs[i+1])
			}
			require.NoError(t, txn.Commit(context.Background()))
			writeTs = append(writeTs, txn.GetPrepareTS())
		}
		wg.Done()
	}()

	// concurrent read to test race
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			for i := 0; i < 10; i++ {
				reader := logMgr.GetReader(minTs, maxTs)
				_ = reader.GetDirtyByTable(dbID, tableID)
			}
			wg.Done()
		}()
	}

	wg.Wait()

	firstWriteTs, lastWriteTs := writeTs[0], writeTs[len(writeTs)-1]

	reader = logMgr.GetReader(firstWriteTs, lastWriteTs.Next())
	require.False(t, reader.HasCatalogChanges())
	reader = logMgr.GetReader(minTs, catalogWriteTs)
	require.Equal(t, 0, len(reader.GetDirtyByTable(dbID, tableID).Segs))
	reader = logMgr.GetReader(firstWriteTs, lastWriteTs)
	require.Equal(t, 0, len(reader.GetDirtyByTable(dbID, tableID-1).Segs))
	// 5 segments, every segment has 2 blocks
	reader = logMgr.GetReader(firstWriteTs, lastWriteTs)
	dirties := reader.GetDirtyByTable(dbID, tableID)
	require.Equal(t, 5, len(dirties.Segs))
	for _, seg := range dirties.Segs {
		require.Equal(t, 2, len(seg.Blks))
	}
	tots := func(ts types.TS) *timestamp.Timestamp {
		return &timestamp.Timestamp{PhysicalTime: types.DecodeInt64(ts[4:12]), LogicalTime: types.DecodeUint32(ts[:4])}
	}

	fixedColCnt := 2 // __rowid + commit_time, the columns for a delBatch
	// check Bat rows count consistency
	check_same_rows := func(bat *api.Batch, expect int) {
		for i, vec := range bat.Vecs {
			col, err := vector.ProtoVectorToVector(vec)
			require.NoError(t, err)
			require.Equal(t, expect, col.Length(), "columns %d", i)
		}
	}

	// get db catalog change
	resp, close, err := logtail.HandleSyncLogTailReq(ctx, new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(minTs),
		CnWant: tots(catalogDropTs),
		Table:  &api.TableID{DbId: pkgcatalog.MO_CATALOG_ID, TbId: pkgcatalog.MO_DATABASE_ID},
	}, true)
	require.NoError(t, err)
	require.Equal(t, 2, len(resp.Commands)) // insert and delete

	require.Equal(t, api.Entry_Insert, resp.Commands[0].EntryType)
	require.Equal(t, len(catalog.SystemDBSchema.ColDefs)+fixedColCnt, len(resp.Commands[0].Bat.Vecs))
	check_same_rows(resp.Commands[0].Bat, 2)                                 // 2 db
	datname, err := vector.ProtoVectorToVector(resp.Commands[0].Bat.Vecs[3]) // datname column
	require.NoError(t, err)
	require.Equal(t, "todrop", datname.GetStringAt(0))
	require.Equal(t, "db", datname.GetStringAt(1))

	require.Equal(t, api.Entry_Delete, resp.Commands[1].EntryType)
	require.Equal(t, fixedColCnt+1, len(resp.Commands[1].Bat.Vecs))
	check_same_rows(resp.Commands[1].Bat, 1) // 1 drop db

	close()

	// get table catalog change
	resp, close, err = logtail.HandleSyncLogTailReq(ctx, new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(minTs),
		CnWant: tots(catalogDropTs),
		Table:  &api.TableID{DbId: pkgcatalog.MO_CATALOG_ID, TbId: pkgcatalog.MO_TABLES_ID},
	}, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(resp.Commands)) // insert
	require.Equal(t, api.Entry_Insert, resp.Commands[0].EntryType)
	require.Equal(t, len(catalog.SystemTableSchema.ColDefs)+fixedColCnt, len(resp.Commands[0].Bat.Vecs))
	check_same_rows(resp.Commands[0].Bat, 2)                                 // 2 tables
	relname, err := vector.ProtoVectorToVector(resp.Commands[0].Bat.Vecs[3]) // relname column
	require.NoError(t, err)
	require.Equal(t, schema.Name, relname.GetStringAt(0))
	require.Equal(t, schema.Name, relname.GetStringAt(1))
	close()

	// get columns catalog change
	resp, close, err = logtail.HandleSyncLogTailReq(ctx, new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(minTs),
		CnWant: tots(catalogDropTs),
		Table:  &api.TableID{DbId: pkgcatalog.MO_CATALOG_ID, TbId: pkgcatalog.MO_COLUMNS_ID},
	}, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(resp.Commands)) // insert
	require.Equal(t, api.Entry_Insert, resp.Commands[0].EntryType)
	require.Equal(t, len(catalog.SystemColumnSchema.ColDefs)+fixedColCnt, len(resp.Commands[0].Bat.Vecs))
	// sysColumnsCount := len(catalog.SystemDBSchema.ColDefs) + len(catalog.SystemTableSchema.ColDefs) + len(catalog.SystemColumnSchema.ColDefs)
	check_same_rows(resp.Commands[0].Bat, len(schema.ColDefs)*2) // column count of 2 tables
	close()

	// get user table change
	resp, close, err = logtail.HandleSyncLogTailReq(ctx, new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(firstWriteTs.Next()), // skip the first write deliberately,
		CnWant: tots(lastWriteTs),
		Table:  &api.TableID{DbId: dbID, TbId: tableID},
	}, true)
	require.NoError(t, err)
	require.Equal(t, 2, len(resp.Commands)) // insert data and delete data

	// blk meta change
	// blkMetaEntry := resp.Commands[0]
	// require.Equal(t, api.Entry_Insert, blkMetaEntry.EntryType)
	// require.Equal(t, len(logtail.BlkMetaSchema.ColDefs)+fixedColCnt, len(blkMetaEntry.Bat.Vecs))
	// check_same_rows(blkMetaEntry.Bat, 9) // 9 blocks, because the first write is excluded.

	// check data change
	insDataEntry := resp.Commands[0]
	require.Equal(t, api.Entry_Insert, insDataEntry.EntryType)
	require.Equal(t, len(schema.ColDefs)+1, len(insDataEntry.Bat.Vecs)) // 5 columns, rowid + commit ts + 2 visibile
	check_same_rows(insDataEntry.Bat, 99)                               // 99 rows, because the first write is excluded.
	// test first user col, this is probably fragile, it depends on the details of MockSchema
	// if something changes, delete this is okay.
	firstCol, err := vector.ProtoVectorToVector(insDataEntry.Bat.Vecs[2]) // mock_0 column, int8 type
	require.Equal(t, types.T_int8, firstCol.GetType().Oid)
	require.NoError(t, err)

	delDataEntry := resp.Commands[1]
	require.Equal(t, api.Entry_Delete, delDataEntry.EntryType)
	require.Equal(t, fixedColCnt+1, len(delDataEntry.Bat.Vecs)) // 3 columns, rowid + commit_ts + aborted
	check_same_rows(delDataEntry.Bat, 10)

	// check delete rowids are exactly what we want
	rowids, err := vector.ProtoVectorToVector(delDataEntry.Bat.Vecs[0])
	require.NoError(t, err)
	require.Equal(t, types.T_Rowid, rowids.GetType().Oid)
	rowidMap := make(map[types.Rowid]int)
	for _, id := range deleteRowIDs {
		rowidMap[id] = 1
	}
	for i := int64(0); i < 10; i++ {
		id := vector.MustFixedCol[types.Rowid](rowids)[i]
		rowidMap[id] = rowidMap[id] + 1
	}
	require.Equal(t, 10, len(rowidMap))
	for _, v := range rowidMap {
		require.Equal(t, 2, v)
	}
	close()
}

// txn1: create relation and append, half blk
// txn2: compact
// txn3: append, shouldn't get rw
func TestGetLastAppender(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(1, -1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 14)
	bats := bat.Split(2)

	tae.CreateRelAndAppend(bats[0], true)
	t.Log(tae.Catalog.SimplePPString(3))

	tae.CompactBlocks(false)
	t.Log(tae.Catalog.SimplePPString(3))

	tae.Restart(ctx)

	txn, rel := tae.GetRelation()
	rel.Append(context.Background(), bats[1])
	require.NoError(t, txn.Commit(context.Background()))
}

// txn1[s1,p1,e1] append1
// txn2[s2,p2,e2] append2
// txn3[s3,p3,e3] append3
// collect [0,p1] [0,p2] [p1+1,p2] [p1+1,p3]
// check data, row count, commit ts
// TODO 1. in2pc committs!=preparets; 2. abort
func TestCollectInsert(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(1, -1)
	schema.BlockMaxRows = 20
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 12)
	bats := bat.Split(4)

	tae.CreateRelAndAppend(bats[0], true)

	txn1, rel := tae.GetRelation()
	assert.NoError(t, rel.Append(context.Background(), bats[1]))
	assert.NoError(t, txn1.Commit(context.Background()))

	p1 := txn1.GetPrepareTS()
	t.Logf("p1= %v", p1.ToString())

	txn2, rel := tae.GetRelation()
	assert.NoError(t, rel.Append(context.Background(), bats[2]))
	assert.NoError(t, txn2.Commit(context.Background()))

	p2 := txn2.GetPrepareTS()
	t.Logf("p2= %v", p2.ToString())

	txn3, rel := tae.GetRelation()
	assert.NoError(t, rel.Append(context.Background(), bats[3]))
	assert.NoError(t, txn3.Commit(context.Background()))

	p3 := txn3.GetPrepareTS()
	t.Logf("p3= %v", p3.ToString())

	_, rel = tae.GetRelation()
	blkit := rel.MakeBlockIt()
	blkdata := blkit.GetBlock().GetMeta().(*catalog.BlockEntry).GetBlockData()

	batch, err := blkdata.CollectAppendInRange(types.TS{}, p1, true)
	assert.NoError(t, err)
	t.Log((batch.Attrs))
	for _, vec := range batch.Vecs {
		t.Log(vec)
		assert.Equal(t, 6, vec.Length())
	}
	batch, err = blkdata.CollectAppendInRange(types.TS{}, p2, true)
	assert.NoError(t, err)
	t.Log((batch.Attrs))
	for _, vec := range batch.Vecs {
		t.Log(vec)
		assert.Equal(t, 9, vec.Length())
	}
	batch, err = blkdata.CollectAppendInRange(p1.Next(), p2, true)
	assert.NoError(t, err)
	t.Log((batch.Attrs))
	for _, vec := range batch.Vecs {
		t.Log(vec)
		assert.Equal(t, 3, vec.Length())
	}
	batch, err = blkdata.CollectAppendInRange(p1.Next(), p3, true)
	assert.NoError(t, err)
	t.Log((batch.Attrs))
	for _, vec := range batch.Vecs {
		t.Log(vec)
		assert.Equal(t, 6, vec.Length())
	}
}

// txn0 append
// txn1[s1,p1,e1] delete
// txn1[s2,p2,e2] delete
// txn1[s3,p3,e3] delete
// collect [0,p1] [0,p2] [p1+1,p2] [p1+1,p3]
func TestCollectDelete(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = 20
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 12)

	tae.CreateRelAndAppend(bat, true)

	_, rel := tae.GetRelation()
	blkit := rel.MakeBlockIt()
	blkID := blkit.GetBlock().GetMeta().(*catalog.BlockEntry).AsCommonID()

	txn1, rel := tae.GetRelation()
	assert.NoError(t, rel.RangeDelete(blkID, 0, 0, handle.DT_Normal))
	assert.NoError(t, txn1.Commit(context.Background()))
	p1 := txn1.GetPrepareTS()
	t.Logf("p1= %v", p1.ToString())

	txn2, rel := tae.GetRelation()
	assert.NoError(t, rel.RangeDelete(blkID, 1, 3, handle.DT_Normal))
	assert.NoError(t, txn2.Commit(context.Background()))
	p2 := txn2.GetPrepareTS()
	t.Logf("p2= %v", p2.ToString())

	txn3, rel := tae.GetRelation()
	assert.NoError(t, rel.RangeDelete(blkID, 4, 5, handle.DT_Normal))
	assert.NoError(t, txn3.Commit(context.Background()))
	p3 := txn3.GetPrepareTS()
	t.Logf("p3= %v", p3.ToString())

	txn, rel := tae.GetRelation()
	blkit = rel.MakeBlockIt()
	blkhandle := blkit.GetBlock()
	blkdata := blkhandle.GetMeta().(*catalog.BlockEntry).GetBlockData()

	batch, err := blkdata.CollectDeleteInRange(context.Background(), types.TS{}, p1, true)
	assert.NoError(t, err)
	t.Logf(logtail.BatchToString("", batch, false))
	for i, vec := range batch.Vecs {
		t.Logf(batch.Attrs[i])
		assert.Equal(t, 1, vec.Length())
	}
	view, err := blkdata.CollectChangesInRange(context.Background(), types.TS{}, p1)
	assert.NoError(t, err)
	t.Logf(view.DeleteMask.String())
	assert.Equal(t, 1, view.DeleteMask.GetCardinality())

	batch, err = blkdata.CollectDeleteInRange(context.Background(), types.TS{}, p2, true)
	assert.NoError(t, err)
	t.Logf(logtail.BatchToString("", batch, false))
	for i, vec := range batch.Vecs {
		t.Logf(batch.Attrs[i])
		assert.Equal(t, 4, vec.Length())
	}
	view, err = blkdata.CollectChangesInRange(context.Background(), types.TS{}, p2)
	assert.NoError(t, err)
	t.Logf(view.DeleteMask.String())
	assert.Equal(t, 4, view.DeleteMask.GetCardinality())

	batch, err = blkdata.CollectDeleteInRange(context.Background(), p1.Next(), p2, true)
	assert.NoError(t, err)
	t.Logf(logtail.BatchToString("", batch, false))
	for i, vec := range batch.Vecs {
		t.Logf(batch.Attrs[i])
		assert.Equal(t, 3, vec.Length())
	}
	view, err = blkdata.CollectChangesInRange(context.Background(), p1.Next(), p2)
	assert.NoError(t, err)
	t.Logf(view.DeleteMask.String())
	assert.Equal(t, 3, view.DeleteMask.GetCardinality())

	batch, err = blkdata.CollectDeleteInRange(context.Background(), p1.Next(), p3, true)
	assert.NoError(t, err)
	t.Logf(logtail.BatchToString("", batch, false))
	for i, vec := range batch.Vecs {
		t.Logf(batch.Attrs[i])
		assert.Equal(t, 5, vec.Length())
	}
	view, err = blkdata.CollectChangesInRange(context.Background(), p1.Next(), p3)
	assert.NoError(t, err)
	t.Logf(view.DeleteMask.String())
	assert.Equal(t, 5, view.DeleteMask.GetCardinality())

	blk1Name := objectio.BuildObjectName(objectio.NewSegmentid(), 0)
	writer, err := blockio.NewBlockWriterNew(tae.Runtime.Fs.Service, blk1Name, 0, nil)
	assert.NoError(t, err)
	writer.SetPrimaryKey(3)
	writer.WriteBatch(containers.ToCNBatch(batch))
	blocks, _, err := writer.Sync(context.TODO())
	assert.NoError(t, err)
	assert.Equal(t, 1, len(blocks))

	deltaLoc := blockio.EncodeLocation(
		writer.GetName(),
		blocks[0].GetExtent(),
		uint32(batch.Length()),
		blocks[0].GetID(),
	)

	err = blkhandle.UpdateDeltaLoc(deltaLoc)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	blkdata.GCInMemeoryDeletesByTS(p3)

	batch, err = blkdata.CollectDeleteInRange(context.Background(), p1.Next(), p3, true)
	assert.NoError(t, err)
	t.Logf(logtail.BatchToString("", batch, false))
	for i, vec := range batch.Vecs {
		t.Logf(batch.Attrs[i])
		assert.Equal(t, 5, vec.Length())
	}
	view, err = blkdata.CollectChangesInRange(context.Background(), p1.Next(), p3)
	assert.NoError(t, err)
	t.Logf(view.DeleteMask.String())
	assert.Equal(t, 5, view.DeleteMask.GetCardinality())

}

func TestAppendnode(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(1, 0)
	schema.BlockMaxRows = 10000
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	appendCnt := 20
	bat := catalog.MockBatch(schema, appendCnt)
	bats := bat.Split(appendCnt)

	tae.CreateRelAndAppend(bats[0], true)
	tae.CheckRowsByScan(1, false)

	var wg sync.WaitGroup
	pool, _ := ants.NewPool(5)
	defer pool.Release()
	worker := func(i int) func() {
		return func() {
			txn, rel := tae.GetRelation()
			row := testutil.GetColumnRowsByScan(t, rel, 0, true)
			err := tae.DoAppendWithTxn(bats[i], txn, true)
			assert.NoError(t, err)
			row2 := testutil.GetColumnRowsByScan(t, rel, 0, true)
			assert.Equal(t, row+1, row2)
			assert.NoError(t, txn.Commit(context.Background()))
			wg.Done()
		}
	}
	for i := 1; i < appendCnt; i++ {
		wg.Add(1)
		pool.Submit(worker(i))
	}
	wg.Wait()
	tae.CheckRowsByScan(appendCnt, true)

	tae.Restart(ctx)
	tae.CheckRowsByScan(appendCnt, true)
}

func TestTxnIdempotent(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(1, 0)
	schema.BlockMaxRows = 10000
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	appendCnt := 20
	bat := catalog.MockBatch(schema, appendCnt)
	bats := bat.Split(appendCnt)

	var wg sync.WaitGroup

	tae.CreateRelAndAppend(bats[0], true)
	for i := 0; i < 10; i++ {
		txn, _ := tae.GetRelation()
		wg.Add(1)
		assert.NoError(t, txn.Rollback(context.Background()))
		go func() {
			defer wg.Done()
			assert.True(t, moerr.IsMoErrCode(txn.Commit(context.Background()), moerr.ErrTxnNotFound))
			// txn.Commit(context.Background())
		}()
		wg.Wait()
	}
}

// insert 200 rows and do quick compaction
// expect that there are some dirty tables at first and then zero dirty table found
func TestWatchDirty(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	logMgr := tae.LogtailMgr

	visitor := &catalog.LoopProcessor{}
	watcher := logtail.NewDirtyCollector(logMgr, opts.Clock, tae.Catalog, visitor)

	tbl, seg, blk := watcher.DirtyCount()
	assert.Zero(t, blk)
	assert.Zero(t, seg)
	assert.Zero(t, tbl)

	schema := catalog.MockSchemaAll(1, 0)
	schema.BlockMaxRows = 50
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	appendCnt := 200
	bat := catalog.MockBatch(schema, appendCnt)
	bats := bat.Split(appendCnt)

	tae.CreateRelAndAppend(bats[0], true)
	tae.CheckRowsByScan(1, false)

	wg := &sync.WaitGroup{}
	pool, _ := ants.NewPool(3)
	defer pool.Release()
	worker := func(i int) func() {
		return func() {
			txn, _ := tae.GetRelation()
			err := tae.DoAppendWithTxn(bats[i], txn, true)
			assert.NoError(t, err)
			assert.NoError(t, txn.Commit(context.Background()))
			wg.Done()
		}
	}
	for i := 1; i < appendCnt; i++ {
		wg.Add(1)
		pool.Submit(worker(i))
	}
	wg.Wait()

	timer := time.After(10 * time.Second)
	for {
		select {
		case <-timer:
			t.Errorf("timeout to wait zero")
			return
		default:
			watcher.Run()
			time.Sleep(5 * time.Millisecond)
			_, _, blkCnt := watcher.DirtyCount()
			// find block zero
			if blkCnt == 0 {
				return
			}
		}
	}
}

func TestDirtyWatchRace(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(2, -1)
	schema.Name = "test"
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 5
	tae.BindSchema(schema)

	tae.CreateRelAndAppend(catalog.MockBatch(schema, 1), true)

	visitor := &catalog.LoopProcessor{}
	watcher := logtail.NewDirtyCollector(tae.LogtailMgr, opts.Clock, tae.Catalog, visitor)

	wg := &sync.WaitGroup{}

	addRow := func() {
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		tbl, _ := db.GetRelationByName("test")
		tbl.Append(context.Background(), catalog.MockBatch(schema, 1))
		assert.NoError(t, txn.Commit(context.Background()))
		wg.Done()
	}

	pool, _ := ants.NewPool(5)
	defer pool.Release()

	for i := 0; i < 50; i++ {
		wg.Add(1)
		pool.Submit(addRow)
	}

	// test race
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			for j := 0; j < 300; j++ {
				time.Sleep(5 * time.Millisecond)
				watcher.Run()
				// tbl, seg, blk := watcher.DirtyCount()
				// t.Logf("t%d: tbl %d, seg %d, blk %d", i, tbl, seg, blk)
				_, _, _ = watcher.DirtyCount()
			}
			wg.Done()
		}(i)
	}

	wg.Wait()
}

func TestBlockRead(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	tsAlloc := types.NewTsAlloctor(opts.Clock)
	defer tae.Close()
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = 20
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 40)

	tae.CreateRelAndAppend(bat, true)

	_, rel := tae.GetRelation()
	blkit := rel.MakeBlockIt()
	blkEntry := blkit.GetBlock().GetMeta().(*catalog.BlockEntry)
	blkID := blkEntry.AsCommonID()

	beforeDel := tsAlloc.Alloc()
	txn1, rel := tae.GetRelation()
	assert.NoError(t, rel.RangeDelete(blkID, 0, 0, handle.DT_Normal))
	assert.NoError(t, txn1.Commit(context.Background()))

	afterFirstDel := tsAlloc.Alloc()
	txn2, rel := tae.GetRelation()
	assert.NoError(t, rel.RangeDelete(blkID, 1, 3, handle.DT_Normal))
	assert.NoError(t, txn2.Commit(context.Background()))

	afterSecondDel := tsAlloc.Alloc()

	tae.CompactBlocks(false)

	metaloc := blkEntry.GetMetaLoc()
	deltaloc := blkEntry.GetDeltaLoc()
	assert.NotEmpty(t, metaloc)
	assert.NotEmpty(t, deltaloc)

	bid, sid := blkEntry.ID, blkEntry.GetSegment().ID

	info := &pkgcatalog.BlockInfo{
		BlockID:    bid,
		SegmentID:  sid,
		EntryState: true,
	}
	info.SetMetaLocation(metaloc)
	info.SetDeltaLocation(deltaloc)

	columns := make([]string, 0)
	colIdxs := make([]uint16, 0)
	colTyps := make([]types.Type, 0)
	defs := schema.ColDefs[:]
	rand.Shuffle(len(defs), func(i, j int) { defs[i], defs[j] = defs[j], defs[i] })
	for _, col := range defs {
		columns = append(columns, col.Name)
		colIdxs = append(colIdxs, uint16(col.Idx))
		colTyps = append(colTyps, col.Type)
	}
	t.Log("read columns: ", columns)
	fs := tae.DB.Runtime.Fs.Service
	pool, err := mpool.NewMPool("test", 0, mpool.NoFixed)
	assert.NoError(t, err)
	infos := make([][]*pkgcatalog.BlockInfo, 0)
	infos = append(infos, []*pkgcatalog.BlockInfo{info})
	err = blockio.BlockPrefetch(colIdxs, fs, infos)
	assert.NoError(t, err)
	b1, err := blockio.BlockReadInner(
		context.Background(), info, nil, colIdxs, colTyps,
		beforeDel, nil, fs, pool, nil,
	)
	assert.NoError(t, err)
	assert.Equal(t, len(columns), len(b1.Vecs))
	assert.Equal(t, 20, b1.Vecs[0].Length())

	b2, err := blockio.BlockReadInner(
		context.Background(), info, nil, colIdxs, colTyps,
		afterFirstDel, nil, fs, pool, nil,
	)
	assert.NoError(t, err)
	assert.Equal(t, 19, b2.Vecs[0].Length())
	b3, err := blockio.BlockReadInner(
		context.Background(), info, nil, colIdxs, colTyps,
		afterSecondDel, nil, fs, pool, nil,
	)
	assert.NoError(t, err)
	assert.Equal(t, len(columns), len(b2.Vecs))
	assert.Equal(t, 16, b3.Vecs[0].Length())

	// read rowid column only
	b4, err := blockio.BlockReadInner(
		context.Background(), info,
		nil,
		[]uint16{2},
		[]types.Type{types.T_Rowid.ToType()},
		afterSecondDel, nil, fs, pool, nil,
	)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(b4.Vecs))
	assert.Equal(t, 16, b4.Vecs[0].Length())

	// read rowid column only
	info.EntryState = false
	b5, err := blockio.BlockReadInner(
		context.Background(), info,
		nil, []uint16{2},
		[]types.Type{types.T_Rowid.ToType()},
		afterSecondDel, nil, fs, pool, nil,
	)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(b5.Vecs))
	assert.Equal(t, 16, b5.Vecs[0].Length())
}

func TestCompactDeltaBlk(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 6
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 5)

	tae.CreateRelAndAppend(bat, true)

	{
		v := testutil.GetSingleSortKeyValue(bat, schema, 1)
		t.Logf("v is %v**********", v)
		filter := handle.NewEQFilter(v)
		txn2, rel := tae.GetRelation()
		t.Log("********before delete******************")
		testutil.CheckAllColRowsByScan(t, rel, 5, true)
		_ = rel.DeleteByFilter(context.Background(), filter)
		assert.Nil(t, txn2.Commit(context.Background()))
	}

	_, rel := tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 4, true)

	{
		t.Log("************compact************")
		txn, rel := tae.GetRelation()
		it := rel.MakeBlockIt()
		blk := it.GetBlock()
		meta := blk.GetMeta().(*catalog.BlockEntry)
		task, err := jobs.NewCompactBlockTask(nil, txn, meta, tae.DB.Runtime)
		assert.NoError(t, err)
		err = task.OnExec(context.Background())
		assert.NoError(t, err)
		assert.True(t, !meta.GetMetaLoc().IsEmpty())
		assert.True(t, !meta.GetDeltaLoc().IsEmpty())
		assert.False(t, task.GetNewBlock().GetMeta().(*catalog.BlockEntry).GetMetaLoc().IsEmpty())
		assert.True(t, task.GetNewBlock().GetMeta().(*catalog.BlockEntry).GetDeltaLoc().IsEmpty())
		err = txn.Commit(context.Background())
		assert.Nil(t, err)
		err = meta.GetSegment().RemoveEntry(meta)
		assert.Nil(t, err)
	}
	{
		v := testutil.GetSingleSortKeyValue(bat, schema, 2)
		t.Logf("v is %v**********", v)
		filter := handle.NewEQFilter(v)
		txn2, rel := tae.GetRelation()
		t.Log("********before delete******************")
		testutil.CheckAllColRowsByScan(t, rel, 4, true)
		_ = rel.DeleteByFilter(context.Background(), filter)
		assert.Nil(t, txn2.Commit(context.Background()))
	}
	{
		t.Log("************compact************")
		txn, rel := tae.GetRelation()
		it := rel.MakeBlockIt()
		blk := it.GetBlock()
		meta := blk.GetMeta().(*catalog.BlockEntry)
		assert.False(t, meta.IsAppendable())
		task, err := jobs.NewCompactBlockTask(nil, txn, meta, tae.DB.Runtime)
		assert.NoError(t, err)
		err = task.OnExec(context.Background())
		assert.NoError(t, err)
		assert.True(t, !meta.GetMetaLoc().IsEmpty())
		assert.True(t, !meta.GetDeltaLoc().IsEmpty())
		assert.False(t, task.GetNewBlock().GetMeta().(*catalog.BlockEntry).GetMetaLoc().IsEmpty())
		assert.True(t, task.GetNewBlock().GetMeta().(*catalog.BlockEntry).GetDeltaLoc().IsEmpty())
		err = txn.Commit(context.Background())
		assert.Nil(t, err)
	}

	_, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 3, true)
	assert.Equal(t, int64(3), rel.Rows())

	tae.Restart(ctx)
	_, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 3, true)
	assert.Equal(t, int64(3), rel.Rows())
}

func TestFlushTable(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	tae.BGCheckpointRunner.DebugUpdateOptions(
		checkpoint.WithForceFlushCheckInterval(time.Millisecond * 5))

	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 21)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)

	_, rel := tae.GetRelation()
	db, err := rel.GetDB()
	assert.Nil(t, err)
	table, err := db.GetRelationByName(schema.Name)
	assert.Nil(t, err)
	err = tae.FlushTable(
		context.Background(),
		0,
		db.GetID(),
		table.ID(),
		types.BuildTS(time.Now().UTC().UnixNano(), 0))
	assert.NoError(t, err)
	t.Log(tae.Catalog.SimplePPString(common.PPL1))

	txn, rel := tae.GetRelation()
	it := rel.MakeBlockIt()
	for it.Valid() {
		blk := it.GetBlock().GetMeta().(*catalog.BlockEntry)
		assert.True(t, blk.HasPersistedData())
		it.Next()
	}
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestReadCheckpoint(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 21)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)
	now := time.Now()
	testutils.WaitExpect(10000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	t.Log(time.Since(now))
	t.Logf("Checkpointed: %d", tae.Runtime.Scheduler.GetCheckpointedLSN())
	t.Logf("GetPenddingLSNCnt: %d", tae.Runtime.Scheduler.GetPenddingLSNCnt())
	assert.Equal(t, uint64(0), tae.Runtime.Scheduler.GetPenddingLSNCnt())
	tids := []uint64{
		pkgcatalog.MO_DATABASE_ID,
		pkgcatalog.MO_TABLES_ID,
		pkgcatalog.MO_COLUMNS_ID,
		1000,
	}

	now = time.Now()
	testutils.WaitExpect(10000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	t.Log(time.Since(now))
	assert.Equal(t, uint64(0), tae.Runtime.Scheduler.GetPenddingLSNCnt())

	now = time.Now()
	testutils.WaitExpect(10000, func() bool {
		return tae.BGCheckpointRunner.GetPenddingIncrementalCount() == 0
	})
	t.Log(time.Since(now))
	assert.Equal(t, 0, tae.BGCheckpointRunner.GetPenddingIncrementalCount())

	gcTS := types.BuildTS(time.Now().UTC().UnixNano(), 0)
	err := tae.BGCheckpointRunner.GCByTS(context.Background(), gcTS)
	assert.NoError(t, err)
	now = time.Now()
	assert.Equal(t, uint64(0), tae.Wal.GetPenddingCnt())
	testutils.WaitExpect(10000, func() bool {
		tae.BGCheckpointRunner.ExistPendingEntryToGC()
		return !tae.BGCheckpointRunner.ExistPendingEntryToGC()
	})
	t.Log(time.Since(now))
	assert.False(t, tae.BGCheckpointRunner.ExistPendingEntryToGC())
	entries := tae.BGCheckpointRunner.GetAllGlobalCheckpoints()
	for _, entry := range entries {
		t.Log(entry.String())
	}
	for _, entry := range entries {
		for _, tid := range tids {
			ins, del, _, _, err := entry.GetByTableID(context.Background(), tae.Runtime.Fs, tid)
			assert.NoError(t, err)
			t.Logf("table %d", tid)
			if ins != nil {
				t.Log(common.ApiBatchToString(ins, 3))
			}
			if del != nil {
				t.Log(common.ApiBatchToString(del, 3))
			}
		}
	}
	tae.Restart(ctx)
	entries = tae.BGCheckpointRunner.GetAllGlobalCheckpoints()
	for _, entry := range entries {
		for _, tid := range tids {
			ins, del, _, _, err := entry.GetByTableID(context.Background(), tae.Runtime.Fs, tid)
			assert.NoError(t, err)
			t.Logf("table %d", tid)
			if ins != nil {
				t.Log(common.ApiBatchToString(ins, 3))
			}
			if del != nil {
				t.Log(common.ApiBatchToString(del, 3))
			}
		}
	}
}

func TestDelete4(t *testing.T) {
	t.Skip(any("This case crashes occasionally, is being fixed, skip it for now"))
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.NewEmptySchema("xx")
	schema.AppendPKCol("name", types.T_varchar.ToType(), 0)
	schema.AppendCol("offset", types.T_uint32.ToType())
	schema.Finalize(false)
	schema.BlockMaxRows = 50
	schema.SegmentMaxBlocks = 5
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 1)
	bat.Vecs[1].Update(0, uint32(0), false)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)

	filter := handle.NewEQFilter(bat.Vecs[0].Get(0))
	var wg sync.WaitGroup
	var count atomic.Uint32

	run := func() {
		defer wg.Done()
		time.Sleep(time.Duration(rand.Intn(20)+1) * time.Millisecond)
		cloneBat := bat.CloneWindow(0, 1)
		defer cloneBat.Close()
		txn, rel := tae.GetRelation()
		id, offset, err := rel.GetByFilter(context.Background(), filter)
		if err != nil {
			txn.Rollback(context.Background())
			return
		}
		v, _, err := rel.GetValue(id, offset, 1)
		if err != nil {
			txn.Rollback(context.Background())
			return
		}
		oldV := v.(uint32)
		newV := oldV + 1
		if err := rel.RangeDelete(id, offset, offset, handle.DT_Normal); err != nil {
			txn.Rollback(context.Background())
			return
		}
		cloneBat.Vecs[1].Update(0, newV, false)
		if err := rel.Append(context.Background(), cloneBat); err != nil {
			txn.Rollback(context.Background())
			return
		}
		if err := txn.Commit(context.Background()); err == nil {
			ok := count.CompareAndSwap(oldV, newV)
			for !ok {
				ok = count.CompareAndSwap(oldV, newV)
			}
			t.Logf("RangeDelete block-%d, offset-%d, old %d newV %d, %s", id.BlockID, offset, oldV, newV, txn.GetCommitTS().ToString())
		}
	}

	p, _ := ants.NewPool(20)
	defer p.Release()
	for i := 0; i < 100; i++ {
		wg.Add(1)
		_ = p.Submit(run)
	}
	wg.Wait()

	t.Logf("count=%v", count.Load())

	getValueFn := func() {
		txn, rel := tae.GetRelation()
		v, _, err := rel.GetValueByFilter(context.Background(), filter, 1)
		assert.NoError(t, err)
		assert.Equal(t, int(count.Load()), int(v.(uint32)))
		assert.NoError(t, txn.Commit(context.Background()))
		t.Logf("GetV=%v, %s", v, txn.GetStartTS().ToString())
	}
	scanFn := func() {
		txn, rel := tae.GetRelation()
		it := rel.MakeBlockIt()
		for it.Valid() {
			blk := it.GetBlock()
			view, err := blk.GetColumnDataById(context.Background(), 0)
			assert.NoError(t, err)
			defer view.Close()
			view.ApplyDeletes()
			if view.Length() != 0 {
				t.Logf("block-%d, data=%s", blk.ID(), logtail.ToStringTemplate(view.GetData(), -1))
			}
			it.Next()
		}
		txn.Commit(context.Background())
	}

	for i := 0; i < 20; i++ {
		getValueFn()
		scanFn()

		tae.Restart(ctx)

		getValueFn()
		scanFn()
		for j := 0; j < 100; j++ {
			wg.Add(1)
			p.Submit(run)
		}
		wg.Wait()
	}
	t.Log(tae.Catalog.SimplePPString(common.PPL3))
}

// append, delete, apppend, get start ts, compact, get active row
func TestGetActiveRow(t *testing.T) {
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 1)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)

	txn, rel := tae.GetRelation()
	v := testutil.GetSingleSortKeyValue(bat, schema, 0)
	filter := handle.NewEQFilter(v)
	id, row, err := rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	assert.NoError(t, rel.Append(context.Background(), bat))
	assert.NoError(t, txn.Commit(context.Background()))

	_, rel = tae.GetRelation()
	{
		txn2, rel2 := tae.GetRelation()
		it := rel2.MakeBlockIt()
		blk := it.GetBlock().GetMeta().(*catalog.BlockEntry)
		task, err := jobs.NewCompactBlockTask(nil, txn2, blk, tae.Runtime)
		assert.NoError(t, err)
		err = task.OnExec(context.Background())
		assert.NoError(t, err)
		assert.NoError(t, txn2.Commit(context.Background()))
	}
	filter = handle.NewEQFilter(v)
	_, _, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
}
func TestTransfer(t *testing.T) {
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(5, 3)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 10
	tae.BindSchema(schema)

	bat := catalog.MockBatch(schema, 10)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)

	filter := handle.NewEQFilter(bat.Vecs[3].Get(3))

	txn1, rel1 := tae.GetRelation()
	err := rel1.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)

	meta := rel1.GetMeta().(*catalog.TableEntry)
	err = tae.FlushTable(context.Background(), 0, meta.GetDB().ID, meta.ID,
		types.BuildTS(time.Now().UTC().UnixNano(), 0))
	assert.NoError(t, err)

	err = txn1.Commit(context.Background())
	// assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnRWConflict))
	assert.NoError(t, err)

	txn2, rel2 := tae.GetRelation()
	_, _, err = rel2.GetValueByFilter(context.Background(), filter, 3)
	t.Log(err)
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
	v, _, err := rel2.GetValueByFilter(context.Background(), handle.NewEQFilter(bat.Vecs[3].Get(4)), 2)
	expectV := bat.Vecs[2].Get(4)
	assert.Equal(t, expectV, v)
	assert.NoError(t, err)
	_ = txn2.Commit(context.Background())
}

func TestTransfer2(t *testing.T) {
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(5, 3)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 10
	tae.BindSchema(schema)

	bat := catalog.MockBatch(schema, 200)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)

	filter := handle.NewEQFilter(bat.Vecs[3].Get(3))

	txn1, rel1 := tae.GetRelation()
	err := rel1.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)

	tae.MergeBlocks(false)

	err = txn1.Commit(context.Background())
	// assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnRWConflict))
	assert.NoError(t, err)

	txn2, rel2 := tae.GetRelation()
	_, _, err = rel2.GetValueByFilter(context.Background(), filter, 3)
	t.Log(err)
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
	v, _, err := rel2.GetValueByFilter(context.Background(), handle.NewEQFilter(bat.Vecs[3].Get(4)), 2)
	expectV := bat.Vecs[2].Get(4)
	assert.Equal(t, expectV, v)
	assert.NoError(t, err)
	_ = txn2.Commit(context.Background())
}

func TestMergeBlocks3(t *testing.T) {
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(5, 3)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 5
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 100)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)
	filter5 := handle.NewEQFilter(bat.Vecs[3].Get(15))
	filter9 := handle.NewEQFilter(bat.Vecs[3].Get(19))
	filter8 := handle.NewEQFilter(bat.Vecs[3].Get(18))
	filter7 := handle.NewEQFilter(bat.Vecs[3].Get(17))
	// delete all rows in first blk in seg1 and the 5th,9th rows in blk2
	{
		txn, rel := tae.GetRelation()
		segit := rel.MakeSegmentIt()
		seg1 := segit.GetSegment().GetMeta().(*catalog.SegmentEntry)
		segHandle, err := rel.GetSegment(&seg1.ID)
		require.NoError(t, err)

		blk1it := segHandle.MakeBlockIt()
		blk11Handle := blk1it.GetBlock()
		view, err := blk11Handle.GetColumnDataByName(context.Background(), catalog.PhyAddrColumnName)
		view.GetData()
		require.NoError(t, err)
		err = rel.DeleteByPhyAddrKeys(view.GetData(), nil)
		require.NoError(t, err)

		require.NoError(t, rel.DeleteByFilter(context.Background(), filter5))
		require.NoError(t, rel.DeleteByFilter(context.Background(), filter9))
		require.NoError(t, txn.Commit(context.Background()))
	}

	// 1. merge first segment
	// 2. delete 7th row in blk2 during executing merge task
	// 3. delete 8th row in blk2 and commit that after merging, test transfer
	{
		del8txn, rel8 := tae.GetRelation()
		valrow8, null, err := rel8.GetValueByFilter(context.Background(), filter8, schema.GetColIdx(catalog.PhyAddrColumnName))
		require.NoError(t, err)
		require.False(t, null)

		del7txn, rel7 := tae.GetRelation()
		mergetxn, relm := tae.GetRelation()

		// merge first segment
		segit := relm.MakeSegmentIt()
		seg1 := segit.GetSegment().GetMeta().(*catalog.SegmentEntry)
		segHandle, err := relm.GetSegment(&seg1.ID)
		require.NoError(t, err)
		metas := make([]*catalog.BlockEntry, 0, 10)
		it := segHandle.MakeBlockIt()
		for ; it.Valid(); it.Next() {
			meta := it.GetBlock().GetMeta().(*catalog.BlockEntry)
			metas = append(metas, meta)
		}
		segsToMerge := []*catalog.SegmentEntry{seg1}
		task, err := jobs.NewMergeBlocksTask(nil, mergetxn, metas, segsToMerge, nil, tae.Runtime)
		require.NoError(t, err)
		require.NoError(t, task.OnExec(context.Background()))

		// delete del7 after starting merge txn
		require.NoError(t, rel7.DeleteByFilter(context.Background(), filter7))
		require.NoError(t, del7txn.Commit(context.Background()))

		// commit merge, and it will carry del7 to the new block
		require.NoError(t, mergetxn.Commit(context.Background()))

		// delete 8 row and it is expected to be transfered correctly
		rel8.DeleteByPhyAddrKey(valrow8)
		require.NoError(t, del8txn.Commit(context.Background()))
	}

	// consistency check
	{
		var err error
		txn, rel := tae.GetRelation()
		_, _, err = rel.GetValueByFilter(context.Background(), filter5, 3)
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		_, _, err = rel.GetValueByFilter(context.Background(), filter7, 3)
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		_, _, err = rel.GetValueByFilter(context.Background(), filter8, 3)
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))
		_, _, err = rel.GetValueByFilter(context.Background(), filter9, 3)
		assert.True(t, moerr.IsMoErrCode(err, moerr.ErrNotFound))

		testutil.CheckAllColRowsByScan(t, rel, 86, true)
		require.NoError(t, txn.Commit(context.Background()))
	}

}

func TestCompactEmptyBlock(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(1, 0)
	schema.BlockMaxRows = 3
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 6)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)
	assert.NoError(t, tae.DeleteAll(true))
	tae.CheckRowsByScan(0, true)

	tae.CompactBlocks(false)

	tae.CheckRowsByScan(0, true)

	blkCnt := 0
	p := &catalog.LoopProcessor{}
	p.BlockFn = func(be *catalog.BlockEntry) error {
		blkCnt++
		return nil
	}

	_, rel := tae.GetRelation()
	err := rel.GetMeta().(*catalog.TableEntry).RecurLoop(p)
	assert.NoError(t, err)
	assert.Equal(t, 2, blkCnt)
	t.Log(tae.Catalog.SimplePPString(3))

	tae.Restart(ctx)

	blkCnt = 0
	_, rel = tae.GetRelation()
	err = rel.GetMeta().(*catalog.TableEntry).RecurLoop(p)
	assert.NoError(t, err)
	assert.Equal(t, 2, blkCnt)
	tae.CheckRowsByScan(0, true)
	t.Log(tae.Catalog.SimplePPString(3))
}

func TestTransfer3(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(5, 3)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 10
	tae.BindSchema(schema)

	bat := catalog.MockBatch(schema, 10)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)

	filter := handle.NewEQFilter(bat.Vecs[3].Get(3))

	txn1, rel1 := tae.GetRelation()

	var err error
	err = rel1.DeleteByFilter(context.Background(), filter)
	assert.NoError(t, err)

	meta := rel1.GetMeta().(*catalog.TableEntry)
	err = tae.FlushTable(context.Background(), 0, meta.GetDB().ID, meta.ID,
		types.BuildTS(time.Now().UTC().UnixNano(), 0))
	assert.NoError(t, err)

	err = rel1.Append(context.Background(), bat.Window(3, 1))
	assert.NoError(t, err)
	err = txn1.Commit(context.Background())
	assert.NoError(t, err)
}

func TestUpdate(t *testing.T) {
	t.Skip(any("This case crashes occasionally, is being fixed, skip it for now"))
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts2(nil, 5)
	// opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(5, 3)
	schema.BlockMaxRows = 100
	schema.SegmentMaxBlocks = 4
	tae.BindSchema(schema)

	bat := catalog.MockBatch(schema, 1)
	defer bat.Close()
	bat.Vecs[2].Update(0, int32(0), false)

	tae.CreateRelAndAppend(bat, true)

	var wg sync.WaitGroup

	var expectV atomic.Int32
	expectV.Store(bat.Vecs[2].Get(0).(int32))
	filter := handle.NewEQFilter(bat.Vecs[3].Get(0))
	updateFn := func() {
		defer wg.Done()
		txn, rel := tae.GetRelation()
		id, offset, err := rel.GetByFilter(context.Background(), filter)
		assert.NoError(t, err)
		v, _, err := rel.GetValue(id, offset, 2)
		assert.NoError(t, err)
		err = rel.RangeDelete(id, offset, offset, handle.DT_Normal)
		if err != nil {
			t.Logf("range delete %v, rollbacking", err)
			_ = txn.Rollback(context.Background())
			return
		}
		tuples := bat.CloneWindow(0, 1)
		defer tuples.Close()
		updatedV := v.(int32) + 1
		tuples.Vecs[2].Update(0, updatedV, false)
		err = rel.Append(context.Background(), tuples)
		assert.NoError(t, err)

		err = txn.Commit(context.Background())
		if err != nil {
			t.Logf("commit update %v", err)
		} else {
			expectV.CompareAndSwap(v.(int32), updatedV)
			t.Logf("%v committed", updatedV)
		}
	}
	p, _ := ants.NewPool(5)
	defer p.Release()
	loop := 1000
	for i := 0; i < loop; i++ {
		wg.Add(1)
		// updateFn()
		_ = p.Submit(updateFn)
	}
	wg.Wait()
	t.Logf("Final: %v", expectV.Load())
	{
		txn, rel := tae.GetRelation()
		v, _, err := rel.GetValueByFilter(context.Background(), filter, 2)
		assert.NoError(t, err)
		assert.Equal(t, v.(int32), expectV.Load())
		testutil.CheckAllColRowsByScan(t, rel, 1, true)
		assert.NoError(t, txn.Commit(context.Background()))
	}
}

// This is used to observe a lot of compactions to overflow a segment, it is not compulsory
func TestAlwaysUpdate(t *testing.T) {
	t.Skip("This is a long test, run it manully to observe catalog")
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts2(nil, 100)
	opts.GCCfg.ScanGCInterval = 3600 * time.Second
	opts.CatalogCfg.GCInterval = 3600 * time.Second
	// opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(5, 3)
	schema.Name = "testupdate"
	schema.BlockMaxRows = 8192
	schema.SegmentMaxBlocks = 200
	tae.BindSchema(schema)

	bats := catalog.MockBatch(schema, 400*100).Split(100)
	metalocs := make([]objectio.Location, 0, 100)
	// write only one segment
	for i := 0; i < 1; i++ {
		objName1 := objectio.NewSegmentid().ToString() + "-0"
		writer, err := blockio.NewBlockWriter(tae.Runtime.Fs.Service, objName1)
		assert.Nil(t, err)
		writer.SetPrimaryKey(3)
		for _, bat := range bats[i*25 : (i+1)*25] {
			_, err := writer.WriteBatch(containers.ToCNBatch(bat))
			assert.Nil(t, err)
		}
		blocks, _, err := writer.Sync(context.Background())
		assert.Nil(t, err)
		assert.Equal(t, 25, len(blocks))
		for _, blk := range blocks {
			loc := blockio.EncodeLocation(writer.GetName(), blk.GetExtent(), 8192, blocks[0].GetID())
			assert.Nil(t, err)
			metalocs = append(metalocs, loc)
		}
	}

	txn, _ := tae.StartTxn(nil)
	db, err := txn.CreateDatabase("db", "", "")
	assert.NoError(t, err)
	tbl, err := db.CreateRelation(schema)
	assert.NoError(t, err)
	assert.NoError(t, tbl.AddBlksWithMetaLoc(context.Background(), metalocs))
	assert.NoError(t, txn.Commit(context.Background()))

	t.Log(tae.Catalog.SimplePPString(common.PPL1))

	wg := &sync.WaitGroup{}

	updateFn := func(i, j int) {
		defer wg.Done()
		tuples := bats[0].CloneWindow(0, 1)
		defer tuples.Close()
		for x := i; x < j; x++ {
			txn, rel := tae.GetRelation()
			filter := handle.NewEQFilter(int64(x))
			id, offset, err := rel.GetByFilter(context.Background(), filter)
			assert.NoError(t, err)
			_, _, err = rel.GetValue(id, offset, 2)
			assert.NoError(t, err)
			err = rel.RangeDelete(id, offset, offset, handle.DT_Normal)
			if err != nil {
				t.Logf("range delete %v, rollbacking", err)
				_ = txn.Rollback(context.Background())
				return
			}
			tuples.Vecs[3].Update(0, int64(x), false)
			err = rel.Append(context.Background(), tuples)
			assert.NoError(t, err)
			assert.NoError(t, txn.Commit(context.Background()))
		}
		t.Logf("(%d, %d) done", i, j)
	}

	p, _ := ants.NewPool(10)
	defer p.Release()

	ch := make(chan int, 1)
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				t.Log(tae.Catalog.SimplePPString(common.PPL1))
			case <-ch:
			}
		}
	}()

	for r := 0; r < 10; r++ {
		for i := 0; i < 40; i++ {
			wg.Add(1)
			start, end := i*200, (i+1)*200
			f := func() { updateFn(start, end) }
			p.Submit(f)
		}
		wg.Wait()
	}
}

func TestInsertPerf(t *testing.T) {
	t.Skip(any("for debug"))
	ctx := context.Background()

	opts := new(options.Options)
	options.WithCheckpointScanInterval(time.Second * 10)(opts)
	options.WithFlushInterval(time.Second * 10)(opts)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(10, 2)
	schema.BlockMaxRows = 1000
	schema.SegmentMaxBlocks = 5
	tae.BindSchema(schema)

	cnt := 1000
	iBat := 1
	poolSize := 20

	bat := catalog.MockBatch(schema, cnt*iBat*poolSize*2)
	defer bat.Close()

	tae.CreateRelAndAppend(bat.Window(0, 1), true)
	var wg sync.WaitGroup
	run := func(start int) func() {
		return func() {
			defer wg.Done()
			for i := start; i < start+cnt*iBat; i += iBat {
				txn, rel := tae.GetRelation()
				_ = rel.Append(context.Background(), bat.Window(i, iBat))
				_ = txn.Commit(context.Background())
			}
		}
	}

	p, _ := ants.NewPool(poolSize)
	defer p.Release()
	now := time.Now()
	for i := 1; i <= poolSize; i++ {
		wg.Add(1)
		_ = p.Submit(run(i * cnt * iBat))
	}
	wg.Wait()
	t.Log(time.Since(now))
}

func TestAppendBat(t *testing.T) {
	p, _ := ants.NewPool(100)
	defer p.Release()
	var wg sync.WaitGroup

	schema := catalog.MockSchema(7, 2)
	bat := catalog.MockBatch(schema, 1000)
	defer bat.Close()

	run := func() {
		defer wg.Done()
		b := containers.BuildBatch(schema.Attrs(), schema.Types(), containers.Options{
			Allocator: common.DefaultAllocator})
		defer b.Close()
		for i := 0; i < bat.Length(); i++ {
			w := bat.Window(i, 1)
			b.Extend(w)
		}
	}

	for i := 0; i < 200; i++ {
		wg.Add(1)
		_ = p.Submit(run)
	}
	wg.Wait()
}

func TestGCWithCheckpoint(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPAndGCOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	manager := gc.NewDiskCleaner(context.Background(), tae.Runtime.Fs, tae.BGCheckpointRunner, tae.Catalog)
	manager.Start()
	defer manager.Stop()

	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 21)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)
	now := time.Now()
	testutils.WaitExpect(10000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	t.Log(time.Since(now))
	t.Logf("Checkpointed: %d", tae.Runtime.Scheduler.GetCheckpointedLSN())
	t.Logf("GetPenddingLSNCnt: %d", tae.Runtime.Scheduler.GetPenddingLSNCnt())
	assert.Equal(t, uint64(0), tae.Runtime.Scheduler.GetPenddingLSNCnt())
	err := manager.GC(context.Background())
	assert.Nil(t, err)
	entries := tae.BGCheckpointRunner.GetAllIncrementalCheckpoints()
	num := len(entries)
	assert.Greater(t, num, 0)
	testutils.WaitExpect(5000, func() bool {
		if manager.GetMaxConsumed() == nil {
			return false
		}
		return entries[num-1].GetEnd().Equal(manager.GetMaxConsumed().GetEnd())
	})
	assert.True(t, entries[num-1].GetEnd().Equal(manager.GetMaxConsumed().GetEnd()))
	manager2 := gc.NewDiskCleaner(context.Background(), tae.Runtime.Fs, tae.BGCheckpointRunner, tae.Catalog)
	manager2.Start()
	defer manager2.Stop()
	testutils.WaitExpect(5000, func() bool {
		if manager2.GetMaxConsumed() == nil {
			return false
		}
		return entries[num-1].GetEnd().Equal(manager2.GetMaxConsumed().GetEnd())
	})
	assert.True(t, entries[num-1].GetEnd().Equal(manager2.GetMaxConsumed().GetEnd()))
	tables1 := manager.GetInputs()
	tables2 := manager2.GetInputs()
	assert.True(t, tables1.Compare(tables2))
}

func TestGCDropDB(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPAndGCOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	manager := gc.NewDiskCleaner(context.Background(), tae.Runtime.Fs, tae.BGCheckpointRunner, tae.Catalog)
	manager.Start()
	defer manager.Stop()
	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 210)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)
	txn, err := tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err := txn.DropDatabase(testutil.DefaultTestDB)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	assert.Equal(t, txn.GetCommitTS(), db.GetMeta().(*catalog.DBEntry).GetDeleteAt())
	now := time.Now()
	testutils.WaitExpect(10000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	t.Log(time.Since(now))
	err = manager.GC(context.Background())
	assert.Nil(t, err)
	entries := tae.BGCheckpointRunner.GetAllIncrementalCheckpoints()
	num := len(entries)
	assert.Greater(t, num, 0)
	testutils.WaitExpect(5000, func() bool {
		if manager.GetMaxConsumed() == nil {
			return false
		}
		return entries[num-1].GetEnd().Equal(manager.GetMaxConsumed().GetEnd())
	})
	assert.True(t, entries[num-1].GetEnd().Equal(manager.GetMaxConsumed().GetEnd()))
	manager2 := gc.NewDiskCleaner(context.Background(), tae.Runtime.Fs, tae.BGCheckpointRunner, tae.Catalog)
	manager2.Start()
	defer manager2.Stop()
	testutils.WaitExpect(5000, func() bool {
		if manager2.GetMaxConsumed() == nil {
			return false
		}
		return entries[num-1].GetEnd().Equal(manager2.GetMaxConsumed().GetEnd())
	})
	assert.True(t, entries[num-1].GetEnd().Equal(manager2.GetMaxConsumed().GetEnd()))
	tables1 := manager.GetInputs()
	tables2 := manager2.GetInputs()
	assert.True(t, tables1.Compare(tables2))
	tae.Restart(ctx)
}

func TestGCDropTable(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPAndGCOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	manager := gc.NewDiskCleaner(context.Background(), tae.Runtime.Fs, tae.BGCheckpointRunner, tae.Catalog)
	manager.Start()
	defer manager.Stop()
	schema := catalog.MockSchemaAll(3, 1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 210)
	defer bat.Close()
	schema2 := catalog.MockSchemaAll(3, 1)
	schema2.BlockMaxRows = 10
	schema2.SegmentMaxBlocks = 2
	bat2 := catalog.MockBatch(schema2, 210)
	defer bat.Close()

	tae.CreateRelAndAppend(bat, true)
	txn, _ := tae.StartTxn(nil)
	db, err := txn.GetDatabase(testutil.DefaultTestDB)
	assert.Nil(t, err)
	rel, _ := db.CreateRelation(schema2)
	rel.Append(context.Background(), bat2)
	assert.Nil(t, txn.Commit(context.Background()))

	txn, err = tae.StartTxn(nil)
	assert.Nil(t, err)
	db, err = txn.GetDatabase(testutil.DefaultTestDB)
	assert.Nil(t, err)
	_, err = db.DropRelationByName(schema2.Name)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit(context.Background()))

	now := time.Now()
	testutils.WaitExpect(10000, func() bool {
		return tae.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	assert.Equal(t, uint64(0), tae.Runtime.Scheduler.GetPenddingLSNCnt())
	assert.Equal(t, txn.GetCommitTS(), rel.GetMeta().(*catalog.TableEntry).GetDeleteAt())
	t.Log(time.Since(now))
	err = manager.GC(context.Background())
	assert.Nil(t, err)
	entries := tae.BGCheckpointRunner.GetAllIncrementalCheckpoints()
	num := len(entries)
	assert.Greater(t, num, 0)
	testutils.WaitExpect(10000, func() bool {
		if manager.GetMaxConsumed() == nil {
			return false
		}
		return entries[num-1].GetEnd().Equal(manager.GetMaxConsumed().GetEnd())
	})
	assert.True(t, entries[num-1].GetEnd().Equal(manager.GetMaxConsumed().GetEnd()))
	manager2 := gc.NewDiskCleaner(context.Background(), tae.Runtime.Fs, tae.BGCheckpointRunner, tae.Catalog)
	manager2.Start()
	defer manager2.Stop()
	testutils.WaitExpect(5000, func() bool {
		if manager2.GetMaxConsumed() == nil {
			return false
		}
		return entries[num-1].GetEnd().Equal(manager2.GetMaxConsumed().GetEnd())
	})
	assert.True(t, entries[num-1].GetEnd().Equal(manager2.GetMaxConsumed().GetEnd()))
	tables1 := manager.GetInputs()
	tables2 := manager2.GetInputs()
	assert.True(t, tables1.Compare(tables2))
	tae.Restart(ctx)
}

func TestAlterRenameTbl(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(2, -1)
	schema.Name = "test"
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	schema.Constraint = []byte("start version")
	schema.Comment = "comment version"

	{
		var err error
		txn, _ := tae.StartTxn(nil)
		txn.CreateDatabase("xx", "", "")
		require.NoError(t, txn.Commit(context.Background()))
		txn1, _ := tae.StartTxn(nil)
		txn2, _ := tae.StartTxn(nil)

		db, _ := txn1.GetDatabase("xx")
		_, err = db.CreateRelation(schema)
		require.NoError(t, err)

		db1, _ := txn2.GetDatabase("xx")
		_, err = db1.CreateRelation(schema)
		require.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnWWConflict))
		require.NoError(t, txn1.Rollback(context.Background()))
		require.NoError(t, txn2.Rollback(context.Background()))
	}

	txn, _ := tae.StartTxn(nil)
	db, _ := txn.CreateDatabase("db", "", "")
	created, _ := db.CreateRelation(schema)
	tid := created.ID()
	txn.Commit(context.Background())

	// concurrent create and in txn alter check
	txn0, _ := tae.StartTxn(nil)
	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	tbl, _ := db.GetRelationByName("test") // 1002
	require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "test", "ultra-test")))
	_, err := db.GetRelationByName("test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	tbl, err = db.GetRelationByName("ultra-test")
	require.NoError(t, err)
	require.Equal(t, tid, tbl.ID())

	require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "ultra-test", "ultraman-test")))
	_, err = db.GetRelationByName("test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	_, err = db.GetRelationByName("ultra-test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	tbl, err = db.GetRelationByName("ultraman-test")
	require.NoError(t, err)
	require.Equal(t, tid, tbl.ID())

	// concurrent txn should see test
	txn1, _ := tae.StartTxn(nil)
	db, err = txn1.GetDatabase("db")
	require.NoError(t, err)
	tbl, err = db.GetRelationByName("test")
	require.NoError(t, err)
	require.Equal(t, tid, tbl.ID())
	_, err = db.GetRelationByName("ultraman-test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	require.NoError(t, txn1.Commit(context.Background()))

	require.NoError(t, txn.Commit(context.Background()))

	txn2, _ := tae.StartTxn(nil)
	db, err = txn2.GetDatabase("db")
	require.NoError(t, err)
	_, err = db.GetRelationByName("test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	_, err = db.GetRelationByName("ultra-test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	tbl, err = db.GetRelationByName("ultraman-test")
	require.NoError(t, err)
	require.Equal(t, tid, tbl.ID())

	require.NoError(t, txn2.Commit(context.Background()))

	// should see test, not newest name
	db, err = txn0.GetDatabase("db")
	require.NoError(t, err)
	_, err = db.GetRelationByName("ultraman-test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	_, err = db.GetRelationByName("ultra-test")
	require.True(t, moerr.IsMoErrCode(err, moerr.OkExpectedEOB))
	tbl, err = db.GetRelationByName("test")
	require.NoError(t, err)
	require.Equal(t, tid, tbl.ID())

	txn3, _ := tae.StartTxn(nil)
	db, _ = txn3.GetDatabase("db")
	rel, err := db.CreateRelation(schema)
	require.NoError(t, err)
	require.NotEqual(t, rel.ID(), tid)
	require.NoError(t, txn3.Commit(context.Background()))

	t.Log(1, db.GetMeta().(*catalog.DBEntry).PrettyNameIndex())
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		tbl, _ := db.GetRelationByName("test")
		require.Error(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "unmatch", "yyyy")))
		require.NoError(t, txn.Rollback(context.Background()))
	}
	// alter back to original schema
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		tbl, _ := db.GetRelationByName("test")
		require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "test", "xx")))
		require.NoError(t, txn.Commit(context.Background()))

		t.Log(2, db.GetMeta().(*catalog.DBEntry).PrettyNameIndex())
		txn, _ = tae.StartTxn(nil)
		db, _ = txn.GetDatabase("db")
		tbl, _ = db.GetRelationByName("xx")
		require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "xx", "test")))
		require.NoError(t, txn.Commit(context.Background()))

		t.Log(3, db.GetMeta().(*catalog.DBEntry).PrettyNameIndex())
	}

	// rename duplicate and rollback
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		schema.Name = "other"
		_, err := db.CreateRelation(schema)
		require.NoError(t, err)
		require.NoError(t, txn.Commit(context.Background()))

		t.Log(4, db.GetMeta().(*catalog.DBEntry).PrettyNameIndex())
		txn, _ = tae.StartTxn(nil)
		db, _ = txn.GetDatabase("db")
		tbl, _ = db.GetRelationByName("test")
		require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "test", "toBeRollback1")))
		require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "toBeRollback1", "toBeRollback2")))
		require.Error(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "toBeRollback2", "other"))) // duplicate
		require.NoError(t, txn.Rollback(context.Background()))

		t.Log(5, db.GetMeta().(*catalog.DBEntry).PrettyNameIndex())
	}

	// test checkpoint replay with txn nil
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		tbl, _ := db.GetRelationByName("test")
		require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "test", "newtest"))) // make test nodelist has no active node
		require.NoError(t, txn.Commit(context.Background()))

		txn, _ = tae.StartTxn(nil)
		db, _ = txn.GetDatabase("db")
		tbl, _ = db.GetRelationByName("other")
		require.NoError(t, tbl.AlterTable(context.TODO(), api.NewRenameTableReq(0, 0, "other", "test"))) // rename other to test, success
		require.NoError(t, txn.Commit(context.Background()))
	}

	tae.Restart(ctx)

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	dbentry := db.GetMeta().(*catalog.DBEntry)
	t.Log(dbentry.PrettyNameIndex())
	require.NoError(t, txn.Commit(context.Background()))

	require.NoError(t, tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.StatMaxCommitTS()))
	tae.Restart(ctx)

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	dbentry = db.GetMeta().(*catalog.DBEntry)
	t.Log(dbentry.PrettyNameIndex())
	require.NoError(t, txn.Commit(context.Background()))
}

func TestAlterRenameTbl2(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(2, -1)
	schema.Name = "t1"
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	schema.Constraint = []byte("start version")
	schema.Comment = "comment version"

	schema2 := schema.Clone()
	schema2.Name = "t1-copy-fefsfwafe"

	schema3 := schema.Clone()
	schema3.Name = "t1-copy-igmgibjtm"

	var oldId, newId uint64
	{
		var err error
		txn, _ := tae.StartTxn(nil)
		txn.CreateDatabase("xx", "", "")

		db, _ := txn.GetDatabase("xx")

		hdl, err := db.CreateRelation(schema)
		require.NoError(t, err)
		oldId = hdl.ID()
		require.NoError(t, txn.Commit(context.Background()))
	}

	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("xx")
		hdl, err := db.CreateRelation(schema2)
		require.NoError(t, err)
		newId = hdl.ID()

		_, err = db.DropRelationByID(oldId)
		require.NoError(t, err)

		newhdl, _ := db.GetRelationByID(newId)
		require.NoError(t, newhdl.AlterTable(ctx, api.NewRenameTableReq(0, 0, "t1-copy-fefsfwafe", "t1")))
		require.NoError(t, txn.Commit(context.Background()))

		dbentry := db.GetMeta().(*catalog.DBEntry)
		t.Log(dbentry.PrettyNameIndex())
	}

	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("xx")
		hdl, err := db.CreateRelation(schema3)
		require.NoError(t, err)
		newId2 := hdl.ID()

		_, err = db.DropRelationByID(newId)
		require.NoError(t, err)

		newhdl, _ := db.GetRelationByID(newId2)
		require.NoError(t, newhdl.AlterTable(ctx, api.NewRenameTableReq(0, 0, "t1-copy-igmgibjtm", "t1")))
		require.NoError(t, txn.Commit(context.Background()))

		dbentry := db.GetMeta().(*catalog.DBEntry)
		t.Log(dbentry.PrettyNameIndex())
		newId = newId2
	}

	tae.Restart(ctx)
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("xx")
		dbentry := db.GetMeta().(*catalog.DBEntry)
		t.Log(dbentry.PrettyNameIndex())
		require.NoError(t, txn.Commit(context.Background()))
	}

	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("xx")

		newhdl, _ := db.GetRelationByID(newId)
		require.NoError(t, newhdl.AlterTable(ctx, api.NewRenameTableReq(0, 0, "t1", "t2")))
		require.NoError(t, txn.Commit(context.Background()))

		dbentry := db.GetMeta().(*catalog.DBEntry)
		t.Log(dbentry.PrettyNameIndex())
	}

	require.NoError(t, tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.StatMaxCommitTS()))

	tae.Restart(ctx)
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("xx")
		dbentry := db.GetMeta().(*catalog.DBEntry)
		t.Log(dbentry.PrettyNameIndex())
		require.NoError(t, txn.Commit(context.Background()))
	}

}

func TestAlterTableBasic(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(2, -1)
	schema.Name = "test"
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	schema.Constraint = []byte("start version")
	schema.Comment = "comment version"

	txn, _ := tae.StartTxn(nil)
	db, _ := txn.CreateDatabase("db", "", "")
	db.CreateRelation(schema)
	txn.Commit(context.Background())

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	tbl, _ := db.GetRelationByName("test")
	err := tbl.AlterTable(context.Background(), api.NewUpdateConstraintReq(0, 0, "version 1"))
	require.NoError(t, err)
	err = tbl.AlterTable(context.Background(), api.NewUpdateCommentReq(0, 0, "comment version 1"))
	require.NoError(t, err)
	err = txn.Commit(context.Background())
	require.NoError(t, err)

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	tbl, _ = db.GetRelationByName("test")
	err = tbl.AlterTable(context.Background(), api.NewUpdateConstraintReq(0, 0, "version 2"))
	require.NoError(t, err)
	txn.Commit(context.Background())

	tots := func(ts types.TS) *timestamp.Timestamp {
		return &timestamp.Timestamp{PhysicalTime: types.DecodeInt64(ts[4:12]), LogicalTime: types.DecodeUint32(ts[:4])}
	}

	resp, close, _ := logtail.HandleSyncLogTailReq(ctx, new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(types.BuildTS(0, 0)),
		CnWant: tots(types.MaxTs()),
		Table:  &api.TableID{DbId: pkgcatalog.MO_CATALOG_ID, TbId: pkgcatalog.MO_TABLES_ID},
	}, true)

	bat, _ := batch.ProtoBatchToBatch(resp.Commands[0].Bat)
	cstrCol := containers.NewNonNullBatchWithSharedMemory(bat).GetVectorByName(pkgcatalog.SystemRelAttr_Constraint)
	require.Equal(t, 3, cstrCol.Length())
	require.Equal(t, []byte("start version"), cstrCol.Get(0).([]byte))
	require.Equal(t, []byte("version 1"), cstrCol.Get(1).([]byte))
	require.Equal(t, []byte("version 2"), cstrCol.Get(2).([]byte))

	commetCol := containers.NewNonNullBatchWithSharedMemory(bat).GetVectorByName(pkgcatalog.SystemRelAttr_Comment)
	require.Equal(t, 3, cstrCol.Length())
	require.Equal(t, []byte("comment version"), commetCol.Get(0).([]byte))
	require.Equal(t, []byte("comment version 1"), commetCol.Get(1).([]byte))
	require.Equal(t, []byte("comment version 1"), commetCol.Get(2).([]byte))

	close()

	tae.Restart(ctx)

	resp, close, _ = logtail.HandleSyncLogTailReq(ctx, new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(types.BuildTS(0, 0)),
		CnWant: tots(types.MaxTs()),
		Table:  &api.TableID{DbId: pkgcatalog.MO_CATALOG_ID, TbId: pkgcatalog.MO_TABLES_ID},
	}, true)

	bat, _ = batch.ProtoBatchToBatch(resp.Commands[0].Bat)
	cstrCol = containers.NewNonNullBatchWithSharedMemory(bat).GetVectorByName(pkgcatalog.SystemRelAttr_Constraint)
	require.Equal(t, 3, cstrCol.Length())
	require.Equal(t, []byte("start version"), cstrCol.Get(0).([]byte))
	require.Equal(t, []byte("version 1"), cstrCol.Get(1).([]byte))
	require.Equal(t, []byte("version 2"), cstrCol.Get(2).([]byte))

	commetCol = containers.NewNonNullBatchWithSharedMemory(bat).GetVectorByName(pkgcatalog.SystemRelAttr_Comment)
	require.Equal(t, 3, cstrCol.Length())
	require.Equal(t, []byte("comment version"), commetCol.Get(0).([]byte))
	require.Equal(t, []byte("comment version 1"), commetCol.Get(1).([]byte))
	require.Equal(t, []byte("comment version 1"), commetCol.Get(2).([]byte))
	close()

	logutil.Info(tae.Catalog.SimplePPString(common.PPL2))

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	_, err = db.DropRelationByName("test")
	require.NoError(t, err)
	txn.Commit(context.Background())

	resp, close, _ = logtail.HandleSyncLogTailReq(ctx, new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(types.BuildTS(0, 0)),
		CnWant: tots(types.MaxTs()),
		Table:  &api.TableID{DbId: pkgcatalog.MO_CATALOG_ID, TbId: pkgcatalog.MO_COLUMNS_ID},
	}, true)

	require.Equal(t, 2, len(resp.Commands)) // create and drop
	require.Equal(t, api.Entry_Insert, resp.Commands[0].EntryType)
	require.Equal(t, api.Entry_Delete, resp.Commands[1].EntryType)
	close()
}

func TestAlterFakePk(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(3, -1)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bats := catalog.MockBatch(schema, 12).Split(3)
	tae.CreateRelAndAppend(bats[0], true)

	var did, tid uint64
	var blkFp *common.ID
	{
		// add two cloumns
		txn, rel := tae.GetRelation()
		tid = rel.ID()
		d, _ := rel.GetDB()
		did = d.GetID()
		blkFp = testutil.GetOneBlock(rel).Fingerprint()
		tblEntry := rel.GetMeta().(*catalog.TableEntry)
		err := rel.AlterTable(context.TODO(), api.NewAddColumnReq(0, 0, "add1", types.NewProtoType(types.T_int32), 1))
		require.NoError(t, err)
		err = rel.AlterTable(context.TODO(), api.NewAddColumnReq(0, 0, "add2", types.NewProtoType(types.T_int64), 2))
		require.NoError(t, err)
		t.Log(tblEntry.StringWithLevel(common.PPL2))
		require.NoError(t, txn.Commit(context.Background()))
		require.Equal(t, 2, tblEntry.MVCC.Depth())
	}

	{
		txn, rel := tae.GetRelation()
		seg, err := rel.GetSegment(blkFp.SegmentID())
		require.NoError(t, err)
		blk, err := seg.GetBlock(blkFp.BlockID)
		require.NoError(t, err)
		err = blk.RangeDelete(1, 1, handle.DT_Normal)
		require.NoError(t, err)
		err = blk.RangeDelete(3, 3, handle.DT_Normal)
		require.NoError(t, err)
		require.NoError(t, txn.Commit(context.Background()))
	}

	{
		txn, rel := tae.GetRelation()
		seg, err := rel.GetSegment(blkFp.SegmentID())
		require.NoError(t, err)
		blk, err := seg.GetBlock(blkFp.BlockID)
		require.NoError(t, err)
		// check non-exist column foreach
		meta := blk.GetMeta().(*catalog.BlockEntry)
		newSchema := meta.GetSchema()
		blkdata := meta.GetBlockData()
		sels := nulls.NewWithSize(4)
		sels.Add(1, 3)
		rows := make([]int, 0, 4)
		blkdata.Foreach(context.Background(), newSchema, 1 /*"add1" column*/, func(v any, isnull bool, row int) error {
			require.True(t, true)
			rows = append(rows, row)
			return nil
		}, sels)
		require.Equal(t, []int{1, 3}, rows)
		require.NoError(t, err)
		require.NoError(t, txn.Commit(context.Background()))
	}

	resp, close, _ := logtail.HandleSyncLogTailReq(context.TODO(), new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(types.BuildTS(0, 0)),
		CnWant: tots(types.MaxTs()),
		Table:  &api.TableID{DbId: did, TbId: tid},
	}, true)

	defer close()
	require.Equal(t, 2, len(resp.Commands)) // first blk 4 insert; first blk 2 dels
	require.Equal(t, api.Entry_Insert, resp.Commands[0].EntryType)
	require.Equal(t, api.Entry_Delete, resp.Commands[1].EntryType)

	insBat, err := batch.ProtoBatchToBatch(resp.Commands[0].Bat)
	require.NoError(t, err)
	dnInsBat := containers.NewNonNullBatchWithSharedMemory(insBat)
	t.Log(dnInsBat.Attrs)
	require.Equal(t, 6, len(dnInsBat.Vecs)) // 3 col + 1 fake pk + 1 rowid + 1 committs
	for _, v := range dnInsBat.Vecs {
		require.Equal(t, 4, v.Length())
	}
	t.Log(dnInsBat.GetVectorByName(pkgcatalog.FakePrimaryKeyColName).PPString(10))

	delBat, err := batch.ProtoBatchToBatch(resp.Commands[1].Bat)
	require.NoError(t, err)
	dnDelBat := containers.NewNonNullBatchWithSharedMemory(delBat)
	t.Log(dnDelBat.Attrs)
	require.Equal(t, 3, len(dnDelBat.Vecs)) // 1 fake pk + 1 rowid + 1 committs
	for _, v := range dnDelBat.Vecs {
		require.Equal(t, 2, v.Length())
	}
	t.Log(dnDelBat.GetVectorByName(pkgcatalog.FakePrimaryKeyColName).PPString(10))

}

func TestAlterColumnAndFreeze(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(10, 5)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bats := catalog.MockBatch(schema, 8).Split(2)
	tae.CreateRelAndAppend(bats[0], true)

	{
		// test error in alter
		txn, rel := tae.GetRelation()
		tblEntry := rel.GetMeta().(*catalog.TableEntry)
		err := rel.AlterTable(context.TODO(), api.NewRemoveColumnReq(0, 0, 1, 10))
		require.True(t, moerr.IsMoErrCode(err, moerr.ErrInternal))
		require.Equal(t, 2, tblEntry.MVCC.Depth())
		t.Log(tblEntry.StringWithLevel(common.PPL2))
		require.NoError(t, txn.Rollback(context.Background()))
		// new node is clean
		require.Equal(t, 1, tblEntry.MVCC.Depth())
	}

	txn0, rel0 := tae.GetRelation()
	db, err := rel0.GetDB()
	require.NoError(t, err)
	did, tid := db.GetID(), rel0.ID()

	require.NoError(t, rel0.Append(context.Background(), bats[1])) // in localsegment

	txn, rel := tae.GetRelation()
	require.NoError(t, rel.AlterTable(context.TODO(), api.NewAddColumnReq(0, 0, "xyz", types.NewProtoType(types.T_int32), 0)))
	require.NoError(t, txn.Commit(context.Background()))

	require.Error(t, rel0.Append(context.Background(), nil)) // schema changed, error
	// Test variaous read on old schema
	testutil.CheckAllColRowsByScan(t, rel0, 8, false)

	filter := handle.NewEQFilter(uint16(3))
	id, row, err := rel0.GetByFilter(context.Background(), filter)
	filen, blkn := id.BlockID.Offsets() // first block
	require.Equal(t, uint16(0), filen)
	require.Equal(t, uint16(0), blkn)
	require.Equal(t, uint32(3), row)
	require.NoError(t, err)

	for _, col := range rel0.Schema().(*catalog.Schema).ColDefs {
		val, null, err := rel0.GetValue(id, 2, uint16(col.Idx))
		require.NoError(t, err)
		require.False(t, null)
		if col.IsPrimary() {
			require.Equal(t, uint16(2), val.(uint16))
		}
	}
	require.Error(t, txn0.Commit(context.Background())) // scheam change, commit failed

	// GetValueByFilter() is combination of GetByFilter and GetValue
	// GetValueByPhyAddrKey is GetValue

	tae.Restart(ctx)

	txn, rel = tae.GetRelation()
	schema1 := rel.Schema().(*catalog.Schema)
	bats = catalog.MockBatch(schema1, 16).Split(4)
	require.Error(t, rel.Append(context.Background(), bats[0])) // dup error
	require.NoError(t, rel.Append(context.Background(), bats[1]))
	require.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	testutil.CheckAllColRowsByScan(t, rel, 8, false)
	it := rel.MakeBlockIt()
	cnt := 0
	var id2 *common.ID
	for ; it.Valid(); it.Next() {
		cnt++
		id2 = it.GetBlock().Fingerprint()
	}
	require.Equal(t, 2, cnt) // 2 blocks because the first is freezed

	for _, col := range rel.Schema().(*catalog.Schema).ColDefs {
		val, null, err := rel.GetValue(id, 3, uint16(col.Idx)) // get first blk
		require.NoError(t, err)
		if col.Name == "xyz" {
			require.True(t, null) // fill null for the new column
		} else {
			require.False(t, null)
		}
		if col.IsPrimary() {
			require.Equal(t, uint16(3), val.(uint16))
		}

		val, null, err = rel.GetValue(id2, 3, uint16(col.Idx)) // get second blk
		require.NoError(t, err)
		require.False(t, null)
		if col.IsPrimary() {
			require.Equal(t, uint16(7), val.(uint16))
		}
	}
	txn.Commit(context.Background())

	// append to the second block
	txn, rel = tae.GetRelation()
	require.NoError(t, rel.Append(context.Background(), bats[2]))
	require.NoError(t, rel.Append(context.Background(), bats[3])) // new block and append 2 rows
	require.NoError(t, txn.Commit(context.Background()))

	// remove and freeze
	txn, rel = tae.GetRelation()
	require.NoError(t, rel.AlterTable(context.TODO(), api.NewRemoveColumnReq(0, 0, 9, 8))) // remove float mock_8
	require.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	schema2 := rel.Schema().(*catalog.Schema)
	bats = catalog.MockBatch(schema2, 20).Split(5)
	require.NoError(t, rel.Append(context.Background(), bats[4])) // new 4th block and append 4 blocks

	testutil.CheckAllColRowsByScan(t, rel, 20, true)
	require.NoError(t, txn.Commit(context.Background()))

	resp, close, _ := logtail.HandleSyncLogTailReq(context.TODO(), new(dummyCpkGetter), tae.LogtailMgr, tae.Catalog, api.SyncLogTailReq{
		CnHave: tots(types.BuildTS(0, 0)),
		CnWant: tots(types.MaxTs()),
		Table:  &api.TableID{DbId: did, TbId: tid},
	}, true)

	require.Equal(t, 3, len(resp.Commands)) // 3 version insert
	bat0 := resp.Commands[0].Bat
	require.Equal(t, 12, len(bat0.Attrs))
	require.Equal(t, "mock_9", bat0.Attrs[2+schema.GetSeqnum("mock_9")])
	bat1 := resp.Commands[1].Bat
	require.Equal(t, 13, len(bat1.Attrs))
	require.Equal(t, "mock_9", bat1.Attrs[2+schema1.GetSeqnum("mock_9")])
	require.Equal(t, "xyz", bat1.Attrs[2+schema1.GetSeqnum("xyz")])
	bat2 := resp.Commands[2].Bat
	require.Equal(t, 13, len(bat2.Attrs))
	require.Equal(t, "mock_9", bat2.Attrs[2+schema1.GetSeqnum("mock_9")])
	require.Equal(t, "mock_9", bat2.Attrs[2+schema2.GetSeqnum("mock_9")])
	require.Equal(t, "xyz", bat2.Attrs[2+schema1.GetSeqnum("xyz")])
	require.Equal(t, "xyz", bat2.Attrs[2+schema2.GetSeqnum("xyz")])
	close()
	logutil.Infof(tae.Catalog.SimplePPString(common.PPL1))
}

func TestGlobalCheckpoint1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	options.WithCheckpointGlobalMinCount(1)(opts)
	options.WithGlobalVersionInterval(time.Millisecond * 10)(opts)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(10, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 400)

	tae.CreateRelAndAppend(bat, true)

	t.Log(tae.Catalog.SimplePPString(common.PPL1))
	tae.Restart(ctx)
	t.Log(tae.Catalog.SimplePPString(common.PPL1))
	tae.CheckRowsByScan(400, true)

	testutils.WaitExpect(4000, func() bool {
		return tae.Wal.GetPenddingCnt() == 0
	})

	tae.Restart(ctx)
	tae.CheckRowsByScan(400, true)
}

func TestAppendAndGC(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := new(options.Options)
	opts.CacheCfg = new(options.CacheCfg)
	opts = config.WithQuickScanAndCKPOpts(opts)
	options.WithDisableGCCheckpoint()(opts)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	db := tae.DB
	db.DiskCleaner.SetMinMergeCountForTest(2)

	schema1 := catalog.MockSchemaAll(13, 2)
	schema1.BlockMaxRows = 10
	schema1.SegmentMaxBlocks = 2

	schema2 := catalog.MockSchemaAll(13, 2)
	schema2.BlockMaxRows = 10
	schema2.SegmentMaxBlocks = 2
	{
		txn, _ := db.StartTxn(nil)
		database, err := txn.CreateDatabase("db", "", "")
		assert.Nil(t, err)
		_, err = database.CreateRelation(schema1)
		assert.Nil(t, err)
		_, err = database.CreateRelation(schema2)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit(context.Background()))
	}
	bat := catalog.MockBatch(schema1, int(schema1.BlockMaxRows*10-1))
	defer bat.Close()
	bats := bat.Split(bat.Length())

	pool, err := ants.NewPool(20)
	assert.Nil(t, err)
	defer pool.Release()
	var wg sync.WaitGroup

	for _, data := range bats {
		wg.Add(2)
		err = pool.Submit(testutil.AppendClosure(t, data, schema1.Name, db, &wg))
		assert.Nil(t, err)
		err = pool.Submit(testutil.AppendClosure(t, data, schema2.Name, db, &wg))
		assert.Nil(t, err)
	}
	wg.Wait()
	testutils.WaitExpect(10000, func() bool {
		return db.Runtime.Scheduler.GetPenddingLSNCnt() == 0
	})
	assert.Equal(t, uint64(0), db.Runtime.Scheduler.GetPenddingLSNCnt())
	err = db.DiskCleaner.CheckGC()
	assert.Nil(t, err)
	testutils.WaitExpect(5000, func() bool {
		return db.DiskCleaner.GetMinMerged() != nil
	})
	minMerged := db.DiskCleaner.GetMinMerged()
	testutils.WaitExpect(5000, func() bool {
		return db.DiskCleaner.GetMinMerged() != nil
	})
	assert.NotNil(t, minMerged)
	tae.Restart(ctx)
	db = tae.DB
	db.DiskCleaner.SetMinMergeCountForTest(2)
	testutils.WaitExpect(5000, func() bool {
		if db.DiskCleaner.GetMaxConsumed() == nil {
			return false
		}
		return db.DiskCleaner.GetMaxConsumed().GetEnd().GreaterEq(minMerged.GetEnd())
	})
	assert.True(t, db.DiskCleaner.GetMaxConsumed().GetEnd().GreaterEq(minMerged.GetEnd()))
	err = db.DiskCleaner.CheckGC()
	assert.Nil(t, err)

}

func TestGlobalCheckpoint2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	options.WithCheckpointGlobalMinCount(1)(opts)
	options.WithDisableGCCatalog()(opts)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	tae.BGCheckpointRunner.DisableCheckpoint()
	tae.BGCheckpointRunner.CleanPenddingCheckpoint()
	defer tae.Close()
	schema := catalog.MockSchemaAll(10, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 40)

	_, firstRel := tae.CreateRelAndAppend(bat, true)

	tae.DropRelation(t)
	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	tae.IncrementalCheckpoint(txn.GetStartTS(), false, true, true)
	tae.GlobalCheckpoint(txn.GetStartTS(), 0, false)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.CreateRelAndAppend(bat, false)

	txn, rel := tae.GetRelation()
	require.NoError(t, rel.AlterTable(context.Background(), api.NewRemoveColumnReq(0, 0, 3, 3)))
	require.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	newschema := rel.Schema().(*catalog.Schema)
	require.Equal(t, uint32(1), newschema.Version)
	require.Equal(t, uint32(10), newschema.Extra.NextColSeqnum)
	require.Equal(t, "mock_3", newschema.Extra.DroppedAttrs[0])
	require.NoError(t, txn.Commit(context.Background()))

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	tae.IncrementalCheckpoint(txn.GetStartTS(), false, true, true)
	tae.GlobalCheckpoint(txn.GetStartTS(), 0, false)
	assert.NoError(t, txn.Commit(context.Background()))

	p := &catalog.LoopProcessor{}
	tableExisted := false
	p.TableFn = func(te *catalog.TableEntry) error {
		if te.ID == firstRel.ID() {
			tableExisted = true
		}
		return nil
	}

	assert.NoError(t, tae.Catalog.RecurLoop(p))
	assert.True(t, tableExisted)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Restart(ctx)
	t.Log(tae.Catalog.SimplePPString(3))

	tableExisted = false
	assert.NoError(t, tae.Catalog.RecurLoop(p))
	assert.False(t, tableExisted)
	txn, rel = tae.GetRelation()
	newschema = rel.Schema().(*catalog.Schema)
	require.Equal(t, uint32(1), newschema.Version)
	require.Equal(t, uint32(10), newschema.Extra.NextColSeqnum)
	require.Equal(t, "mock_3", newschema.Extra.DroppedAttrs[0])
	require.NoError(t, txn.Commit(context.Background()))

}

func TestGlobalCheckpoint3(t *testing.T) {
	t.Skip("This case crashes occasionally, is being fixed, skip it for now")
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	options.WithCheckpointGlobalMinCount(1)(opts)
	options.WithGlobalVersionInterval(time.Nanosecond * 1)(opts)
	options.WithDisableGCCatalog()(opts)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchemaAll(10, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 40)

	_, rel := tae.CreateRelAndAppend(bat, true)
	testutils.WaitExpect(1000, func() bool {
		return tae.Wal.GetPenddingCnt() == 0
	})

	tae.DropRelation(t)
	testutils.WaitExpect(1000, func() bool {
		return tae.Wal.GetPenddingCnt() == 0
	})

	tae.CreateRelAndAppend(bat, false)
	testutils.WaitExpect(1000, func() bool {
		return tae.Wal.GetPenddingCnt() == 0
	})

	p := &catalog.LoopProcessor{}
	tableExisted := false
	p.TableFn = func(te *catalog.TableEntry) error {
		if te.ID == rel.ID() {
			tableExisted = true
		}
		return nil
	}

	assert.NoError(t, tae.Catalog.RecurLoop(p))
	assert.True(t, tableExisted)

	tae.Restart(ctx)

	tableExisted = false
	assert.NoError(t, tae.Catalog.RecurLoop(p))
	assert.False(t, tableExisted)
}

func TestGlobalCheckpoint4(t *testing.T) {
	t.Skip("This case crashes occasionally, is being fixed, skip it for now")
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	tae.BGCheckpointRunner.DisableCheckpoint()
	tae.BGCheckpointRunner.CleanPenddingCheckpoint()
	globalCkpInterval := time.Second

	schema := catalog.MockSchemaAll(18, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 40)

	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	_, err = txn.CreateDatabase("db", "", "")
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	err = tae.IncrementalCheckpoint(txn.GetCommitTS(), false, true, true)
	assert.NoError(t, err)

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	_, err = txn.DropDatabase("db")
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	err = tae.GlobalCheckpoint(txn.GetCommitTS(), globalCkpInterval, false)
	assert.NoError(t, err)

	tae.CreateRelAndAppend(bat, true)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Restart(ctx)
	tae.BGCheckpointRunner.DisableCheckpoint()
	tae.BGCheckpointRunner.CleanPenddingCheckpoint()
	t.Log(tae.Catalog.SimplePPString(3))

	// tae.CreateRelAndAppend(bat, false)

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	db, err := txn.GetDatabase("db")
	assert.NoError(t, err)
	_, err = db.DropRelationByName(schema.Name)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	err = tae.GlobalCheckpoint(txn.GetCommitTS(), globalCkpInterval, false)
	assert.NoError(t, err)

	tae.CreateRelAndAppend(bat, false)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Restart(ctx)
	tae.BGCheckpointRunner.DisableCheckpoint()
	tae.BGCheckpointRunner.CleanPenddingCheckpoint()
	t.Log(tae.Catalog.SimplePPString(3))
}

func TestGlobalCheckpoint5(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	tae.BGCheckpointRunner.DisableCheckpoint()
	tae.BGCheckpointRunner.CleanPenddingCheckpoint()
	globalCkpInterval := time.Duration(0)

	schema := catalog.MockSchemaAll(18, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 60)
	bats := bat.Split(3)

	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	err = tae.IncrementalCheckpoint(txn.GetStartTS(), false, true, true)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.CreateRelAndAppend(bats[0], true)

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	err = tae.GlobalCheckpoint(txn.GetStartTS(), globalCkpInterval, false)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.DoAppend(bats[1])

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	err = tae.GlobalCheckpoint(txn.GetStartTS(), globalCkpInterval, false)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	tae.CheckRowsByScan(40, true)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Restart(ctx)
	tae.BGCheckpointRunner.DisableCheckpoint()
	tae.BGCheckpointRunner.CleanPenddingCheckpoint()
	t.Log(tae.Catalog.SimplePPString(3))

	tae.CheckRowsByScan(40, true)

	tae.DoAppend(bats[2])

	tae.CheckRowsByScan(60, true)
	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	err = tae.GlobalCheckpoint(txn.GetStartTS(), globalCkpInterval, false)
	assert.NoError(t, err)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestGlobalCheckpoint6(t *testing.T) {
	t.Skip("This case crashes occasionally, is being fixed, skip it for now")
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	tae.BGCheckpointRunner.DisableCheckpoint()
	tae.BGCheckpointRunner.CleanPenddingCheckpoint()
	globalCkpInterval := time.Duration(0)
	restartCnt := 10
	batchsize := 10

	schema := catalog.MockSchemaAll(18, 2)
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, batchsize*(restartCnt+1))
	bats := bat.Split(restartCnt + 1)

	tae.CreateRelAndAppend(bats[0], true)
	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	err = tae.IncrementalCheckpoint(txn.GetStartTS(), false, true, true)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	for i := 0; i < restartCnt; i++ {
		tae.DoAppend(bats[i+1])
		txn, err = tae.StartTxn(nil)
		assert.NoError(t, err)
		err = tae.GlobalCheckpoint(txn.GetStartTS(), globalCkpInterval, false)
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit(context.Background()))

		rows := (i + 2) * batchsize
		tae.CheckRowsByScan(rows, true)
		t.Log(tae.Catalog.SimplePPString(3))
		tae.Restart(ctx)
		tae.BGCheckpointRunner.DisableCheckpoint()
		tae.BGCheckpointRunner.CleanPenddingCheckpoint()
		t.Log(tae.Catalog.SimplePPString(3))
		tae.CheckRowsByScan(rows, true)
	}
}

func TestGCCheckpoint1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(18, 2)
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 50)

	tae.CreateRelAndAppend(bat, true)

	testutils.WaitExpect(4000, func() bool {
		return tae.Wal.GetPenddingCnt() == 0
	})
	assert.Equal(t, uint64(0), tae.Wal.GetPenddingCnt())

	testutils.WaitExpect(4000, func() bool {
		return tae.BGCheckpointRunner.GetPenddingIncrementalCount() == 0
	})
	assert.Equal(t, 0, tae.BGCheckpointRunner.GetPenddingIncrementalCount())

	testutils.WaitExpect(4000, func() bool {
		return tae.BGCheckpointRunner.MaxGlobalCheckpoint().IsFinished()
	})
	assert.True(t, tae.BGCheckpointRunner.MaxGlobalCheckpoint().IsFinished())

	tae.BGCheckpointRunner.DisableCheckpoint()

	gcTS := types.BuildTS(time.Now().UTC().UnixNano(), 0)
	t.Log(gcTS.ToString())
	tae.BGCheckpointRunner.GCByTS(context.Background(), gcTS)

	maxGlobal := tae.BGCheckpointRunner.MaxGlobalCheckpoint()

	testutils.WaitExpect(4000, func() bool {
		tae.BGCheckpointRunner.ExistPendingEntryToGC()
		return !tae.BGCheckpointRunner.ExistPendingEntryToGC()
	})
	assert.False(t, tae.BGCheckpointRunner.ExistPendingEntryToGC())

	globals := tae.BGCheckpointRunner.GetAllGlobalCheckpoints()
	assert.Equal(t, 1, len(globals))
	assert.True(t, maxGlobal.GetEnd().Equal(globals[0].GetEnd()))
	for _, global := range globals {
		t.Log(global.String())
	}

	incrementals := tae.BGCheckpointRunner.GetAllIncrementalCheckpoints()
	prevEnd := maxGlobal.GetEnd().Prev()
	for _, incremental := range incrementals {
		assert.True(t, incremental.GetStart().Equal(prevEnd.Next()))
		t.Log(incremental.String())
	}
}

func TestGCCatalog1(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	txn1, _ := tae.StartTxn(nil)
	db, err := txn1.CreateDatabase("db1", "", "")
	assert.Nil(t, err)
	db2, err := txn1.CreateDatabase("db2", "", "")
	assert.Nil(t, err)

	schema := catalog.MockSchema(1, 0)
	schema.Name = "tb1"
	tb, err := db.CreateRelation(schema)
	assert.Nil(t, err)
	schema2 := catalog.MockSchema(1, 0)
	schema2.Name = "tb2"
	tb2, err := db.CreateRelation(schema2)
	assert.Nil(t, err)
	schema3 := catalog.MockSchema(1, 0)
	schema3.Name = "tb3"
	tb3, err := db2.CreateRelation(schema3)
	assert.Nil(t, err)

	seg1, err := tb.CreateSegment(false)
	assert.Nil(t, err)
	seg2, err := tb2.CreateSegment(false)
	assert.Nil(t, err)
	seg3, err := tb2.CreateSegment(false)
	assert.Nil(t, err)
	seg4, err := tb3.CreateSegment(false)
	assert.Nil(t, err)

	_, err = seg1.CreateBlock(false)
	assert.NoError(t, err)
	_, err = seg2.CreateBlock(false)
	assert.NoError(t, err)
	_, err = seg3.CreateBlock(false)
	assert.NoError(t, err)
	blk4, err := seg4.CreateBlock(false)
	assert.NoError(t, err)

	err = txn1.Commit(context.Background())
	assert.Nil(t, err)

	p := &catalog.LoopProcessor{}
	var dbCnt, tableCnt, segCnt, blkCnt int
	p.DatabaseFn = func(d *catalog.DBEntry) error {
		if d.IsSystemDB() {
			return nil
		}
		dbCnt++
		return nil
	}
	p.TableFn = func(te *catalog.TableEntry) error {
		if te.GetDB().IsSystemDB() {
			return nil
		}
		tableCnt++
		return nil
	}
	p.SegmentFn = func(se *catalog.SegmentEntry) error {
		if se.GetTable().GetDB().IsSystemDB() {
			return nil
		}
		segCnt++
		return nil
	}
	p.BlockFn = func(be *catalog.BlockEntry) error {
		if be.GetSegment().GetTable().GetDB().IsSystemDB() {
			return nil
		}
		blkCnt++
		return nil
	}
	resetCount := func() {
		dbCnt = 0
		tableCnt = 0
		segCnt = 0
		blkCnt = 0
	}

	err = tae.Catalog.RecurLoop(p)
	assert.NoError(t, err)
	assert.Equal(t, 2, dbCnt)
	assert.Equal(t, 3, tableCnt)
	assert.Equal(t, 4, segCnt)
	assert.Equal(t, 4, blkCnt)

	txn2, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	db2, err = txn2.GetDatabase("db2")
	assert.NoError(t, err)
	tb3, err = db2.GetRelationByName("tb3")
	assert.NoError(t, err)
	seg4, err = tb3.GetSegment(seg4.GetID())
	assert.NoError(t, err)
	err = seg4.SoftDeleteBlock(blk4.ID())
	assert.NoError(t, err)
	err = txn2.Commit(context.Background())
	assert.NoError(t, err)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Catalog.GCByTS(context.Background(), txn2.GetCommitTS().Next())
	t.Log(tae.Catalog.SimplePPString(3))

	resetCount()
	err = tae.Catalog.RecurLoop(p)
	assert.NoError(t, err)
	assert.Equal(t, 2, dbCnt)
	assert.Equal(t, 3, tableCnt)
	assert.Equal(t, 4, segCnt)
	assert.Equal(t, 3, blkCnt)

	txn3, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	db2, err = txn3.GetDatabase("db2")
	assert.NoError(t, err)
	tb3, err = db2.GetRelationByName("tb3")
	assert.NoError(t, err)
	err = tb3.SoftDeleteSegment(seg4.GetID())
	assert.NoError(t, err)

	db2, err = txn3.GetDatabase("db1")
	assert.NoError(t, err)
	tb3, err = db2.GetRelationByName("tb2")
	assert.NoError(t, err)
	err = tb3.SoftDeleteSegment(seg3.GetID())
	assert.NoError(t, err)

	err = txn3.Commit(context.Background())
	assert.NoError(t, err)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Catalog.GCByTS(context.Background(), txn3.GetCommitTS().Next())
	t.Log(tae.Catalog.SimplePPString(3))

	resetCount()
	err = tae.Catalog.RecurLoop(p)
	assert.NoError(t, err)
	assert.Equal(t, 2, dbCnt)
	assert.Equal(t, 3, tableCnt)
	assert.Equal(t, 2, segCnt)
	assert.Equal(t, 2, blkCnt)

	txn4, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	db2, err = txn4.GetDatabase("db2")
	assert.NoError(t, err)
	_, err = db2.DropRelationByName("tb3")
	assert.NoError(t, err)

	db2, err = txn4.GetDatabase("db1")
	assert.NoError(t, err)
	_, err = db2.DropRelationByName("tb2")
	assert.NoError(t, err)

	err = txn4.Commit(context.Background())
	assert.NoError(t, err)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Catalog.GCByTS(context.Background(), txn4.GetCommitTS().Next())
	t.Log(tae.Catalog.SimplePPString(3))

	resetCount()
	err = tae.Catalog.RecurLoop(p)
	assert.NoError(t, err)
	assert.Equal(t, 2, dbCnt)
	assert.Equal(t, 1, tableCnt)
	assert.Equal(t, 1, segCnt)
	assert.Equal(t, 1, blkCnt)

	txn5, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	_, err = txn5.DropDatabase("db2")
	assert.NoError(t, err)

	_, err = txn5.DropDatabase("db1")
	assert.NoError(t, err)

	err = txn5.Commit(context.Background())
	assert.NoError(t, err)

	t.Log(tae.Catalog.SimplePPString(3))
	tae.Catalog.GCByTS(context.Background(), txn5.GetCommitTS().Next())
	t.Log(tae.Catalog.SimplePPString(3))

	resetCount()
	err = tae.Catalog.RecurLoop(p)
	assert.NoError(t, err)
	assert.Equal(t, 0, dbCnt)
	assert.Equal(t, 0, tableCnt)
	assert.Equal(t, 0, segCnt)
	assert.Equal(t, 0, blkCnt)
}

func TestGCCatalog2(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	options.WithCatalogGCInterval(10 * time.Millisecond)(opts)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchema(3, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 33)

	checkCompactAndGCFn := func() bool {
		p := &catalog.LoopProcessor{}
		appendableCount := 0
		p.BlockFn = func(be *catalog.BlockEntry) error {
			if be.GetSegment().GetTable().GetDB().IsSystemDB() {
				return nil
			}
			if be.IsAppendable() {
				appendableCount++
			}
			return nil
		}
		err := tae.Catalog.RecurLoop(p)
		assert.NoError(t, err)
		return appendableCount == 0
	}

	tae.CreateRelAndAppend(bat, true)
	t.Log(tae.Catalog.SimplePPString(3))
	testutils.WaitExpect(4000, checkCompactAndGCFn)
	assert.True(t, checkCompactAndGCFn())
	t.Log(tae.Catalog.SimplePPString(3))
}
func TestGCCatalog3(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	options.WithCatalogGCInterval(10 * time.Millisecond)(opts)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	schema := catalog.MockSchema(3, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 33)

	checkCompactAndGCFn := func() bool {
		p := &catalog.LoopProcessor{}
		dbCount := 0
		p.DatabaseFn = func(be *catalog.DBEntry) error {
			if be.IsSystemDB() {
				return nil
			}
			dbCount++
			return nil
		}
		err := tae.Catalog.RecurLoop(p)
		assert.NoError(t, err)
		return dbCount == 0
	}

	tae.CreateRelAndAppend(bat, true)
	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	_, err = txn.DropDatabase("db")
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	t.Log(tae.Catalog.SimplePPString(3))
	testutils.WaitExpect(10000, checkCompactAndGCFn)
	assert.True(t, checkCompactAndGCFn())
	t.Log(tae.Catalog.SimplePPString(3))
}

func TestForceCheckpoint(t *testing.T) {
	fault.Enable()
	defer fault.Disable()
	err := fault.AddFaultPoint(context.Background(), "tae: flush timeout", ":::", "echo", 0, "mock flush timeout")
	assert.NoError(t, err)
	defer func() {
		err := fault.RemoveFaultPoint(context.Background(), "tae: flush timeout")
		assert.NoError(t, err)
	}()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(18, 2)
	schema.BlockMaxRows = 5
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 50)

	tae.CreateRelAndAppend(bat, true)

	err = tae.BGCheckpointRunner.ForceFlush(tae.TxnMgr.StatMaxCommitTS(), context.Background(), time.Second)
	assert.Error(t, err)
	err = tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.StatMaxCommitTS())
	assert.NoError(t, err)
}

func TestLogailAppend(t *testing.T) {
	ctx := context.Background()
	tae := testutil.NewTestEngine(ctx, ModuleName, t, nil)
	defer tae.Close()
	tae.DB.LogtailMgr.RegisterCallback(logtail.MockCallback)
	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	tae.BindSchema(schema)
	batch := catalog.MockBatch(schema, int(schema.BlockMaxRows*uint32(schema.SegmentMaxBlocks)-1))
	//create database, create table, append
	tae.CreateRelAndAppend(batch, true)
	//delete
	err := tae.DeleteAll(true)
	assert.NoError(t, err)
	//compact(metadata)
	tae.DoAppend(batch)
	tae.CompactBlocks(false)
	//drop table
	tae.DropRelation(t)
	//drop database
	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	txn.DropDatabase("db")
	assert.NoError(t, txn.Commit(context.Background()))
}

func TestSnapshotLag1(t *testing.T) {
	ctx := context.Background()
	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(14, 3)
	schema.BlockMaxRows = 10000
	schema.SegmentMaxBlocks = 10
	tae.BindSchema(schema)

	data := catalog.MockBatch(schema, 20)
	defer data.Close()

	bats := data.Split(4)
	tae.CreateRelAndAppend(bats[0], true)

	txn1, rel1 := tae.GetRelation()
	assert.NoError(t, rel1.Append(context.Background(), bats[1]))
	txn2, rel2 := tae.GetRelation()
	assert.NoError(t, rel2.Append(context.Background(), bats[1]))

	{
		txn, rel := tae.GetRelation()
		assert.NoError(t, rel.Append(context.Background(), bats[1]))
		assert.NoError(t, txn.Commit(context.Background()))
	}

	txn1.MockStartTS(tae.TxnMgr.Now())
	err := txn1.Commit(context.Background())
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrDuplicateEntry))
	err = txn2.Commit(context.Background())
	assert.True(t, moerr.IsMoErrCode(err, moerr.ErrTxnWWConflict))
}

func TestMarshalPartioned(t *testing.T) {
	ctx := context.Background()
	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(14, 3)
	schema.BlockMaxRows = 10000
	schema.SegmentMaxBlocks = 10
	schema.Partitioned = 1
	tae.BindSchema(schema)

	data := catalog.MockBatch(schema, 20)
	defer data.Close()

	bats := data.Split(4)
	tae.CreateRelAndAppend(bats[0], true)

	_, rel := tae.GetRelation()
	partioned := rel.Schema().(*catalog.Schema).Partitioned
	assert.Equal(t, int8(1), partioned)

	tae.Restart(ctx)

	_, rel = tae.GetRelation()
	partioned = rel.Schema().(*catalog.Schema).Partitioned
	assert.Equal(t, int8(1), partioned)

	err := tae.BGCheckpointRunner.ForceIncrementalCheckpoint(tae.TxnMgr.StatMaxCommitTS())
	assert.NoError(t, err)
	lsn := tae.BGCheckpointRunner.MaxLSNInRange(tae.TxnMgr.StatMaxCommitTS())
	entry, err := tae.Wal.RangeCheckpoint(1, lsn)
	assert.NoError(t, err)
	assert.NoError(t, entry.WaitDone())

	tae.Restart(ctx)

	_, rel = tae.GetRelation()
	partioned = rel.Schema().(*catalog.Schema).Partitioned
	assert.Equal(t, int8(1), partioned)
}

func TestDedup2(t *testing.T) {
	ctx := context.Background()
	opts := config.WithQuickScanAndCKPAndGCOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(14, 3)
	schema.BlockMaxRows = 2
	schema.SegmentMaxBlocks = 10
	schema.Partitioned = 1
	tae.BindSchema(schema)

	count := 50
	data := catalog.MockBatch(schema, count)
	datas := data.Split(count)

	tae.CreateRelAndAppend(datas[0], true)

	for i := 1; i < count; i++ {
		tae.DoAppend(datas[i])
		txn, rel := tae.GetRelation()
		for j := 0; j <= i; j++ {
			err := rel.Append(context.Background(), datas[j])
			assert.Error(t, err)
		}
		assert.NoError(t, txn.Commit(context.Background()))
	}
}

func TestCompactLargeTable(t *testing.T) {
	ctx := context.Background()
	opts := config.WithQuickScanAndCKPAndGCOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(600, 3)
	schema.BlockMaxRows = 2
	schema.SegmentMaxBlocks = 10
	schema.Partitioned = 1
	tae.BindSchema(schema)

	data := catalog.MockBatch(schema, 10)

	tae.CreateRelAndAppend(data, true)

	tae.Restart(ctx)

	tae.CheckRowsByScan(10, true)

	testutils.WaitExpect(10000, func() bool {
		return tae.Wal.GetPenddingCnt() == 0
	})

	tae.Restart(ctx)

	tae.CheckRowsByScan(10, true)
}

func TestCommitS3Blocks(t *testing.T) {
	ctx := context.Background()
	opts := config.WithQuickScanAndCKPAndGCOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(60, 3)
	schema.BlockMaxRows = 20
	schema.SegmentMaxBlocks = 10
	schema.Partitioned = 1
	tae.BindSchema(schema)

	data := catalog.MockBatch(schema, 200)
	datas := data.Split(10)
	tae.CreateRelAndAppend(datas[0], true)
	datas = datas[1:]

	blkMetas := make([]objectio.Location, 0)
	for _, bat := range datas {
		name := objectio.BuildObjectName(objectio.NewSegmentid(), 0)
		writer, err := blockio.NewBlockWriterNew(tae.Runtime.Fs.Service, name, 0, nil)
		assert.Nil(t, err)
		writer.SetPrimaryKey(3)
		for i := 0; i < 50; i++ {
			_, err := writer.WriteBatch(containers.ToCNBatch(bat))
			assert.Nil(t, err)
			//offset++
		}
		blocks, _, err := writer.Sync(context.Background())
		assert.Nil(t, err)
		assert.Equal(t, 50, len(blocks))
		for _, blk := range blocks {
			metaLoc := blockio.EncodeLocation(
				writer.GetName(),
				blk.GetExtent(),
				uint32(bat.Vecs[0].Length()),
				blk.GetID())
			assert.Nil(t, err)
			blkMetas = append(blkMetas, metaLoc)
		}
	}

	for _, meta := range blkMetas {
		txn, rel := tae.GetRelation()
		rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{meta})
		assert.NoError(t, txn.Commit(context.Background()))
	}
	for _, meta := range blkMetas {
		txn, rel := tae.GetRelation()
		err := rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{meta})
		assert.Error(t, err)
		assert.NoError(t, txn.Commit(context.Background()))
	}
}

func TestDedupSnapshot1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(13, 3)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 3
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, 10)
	tae.CreateRelAndAppend(bat, true)

	testutils.WaitExpect(10000, func() bool {
		return tae.Wal.GetPenddingCnt() == 0
	})
	assert.Equal(t, uint64(0), tae.Wal.GetPenddingCnt())

	txn, rel := tae.GetRelation()
	txn.SetSnapshotTS(txn.GetStartTS().Next())
	txn.SetDedupType(txnif.IncrementalDedup)
	err := rel.Append(context.Background(), bat)
	assert.NoError(t, err)
	_ = txn.Commit(context.Background())
}

func TestDedupSnapshot2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(13, 3)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 3
	tae.BindSchema(schema)
	data := catalog.MockBatch(schema, 200)
	testutil.CreateRelation(t, tae.DB, "db", schema, true)

	name := objectio.BuildObjectName(objectio.NewSegmentid(), 0)
	writer, err := blockio.NewBlockWriterNew(tae.Runtime.Fs.Service, name, 0, nil)
	assert.Nil(t, err)
	writer.SetPrimaryKey(3)
	_, err = writer.WriteBatch(containers.ToCNBatch(data))
	assert.Nil(t, err)
	blocks, _, err := writer.Sync(context.Background())
	assert.Nil(t, err)
	assert.Equal(t, 1, len(blocks))
	metaLoc := blockio.EncodeLocation(
		writer.GetName(),
		blocks[0].GetExtent(),
		uint32(data.Vecs[0].Length()),
		blocks[0].GetID())
	assert.Nil(t, err)

	txn, rel := tae.GetRelation()
	err = rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{metaLoc})
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, rel = tae.GetRelation()
	txn.SetSnapshotTS(txn.GetStartTS().Next())
	txn.SetDedupType(txnif.IncrementalDedup)
	err = rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{metaLoc})
	assert.NoError(t, err)
	_ = txn.Commit(context.Background())
}

func TestDedupSnapshot3(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	ctx := context.Background()

	opts := config.WithQuickScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(13, 3)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 3
	tae.BindSchema(schema)
	testutil.CreateRelation(t, tae.DB, "db", schema, true)

	totalRows := 100

	bat := catalog.MockBatch(schema, int(totalRows))
	bats := bat.Split(totalRows)
	var wg sync.WaitGroup
	pool, _ := ants.NewPool(80)
	defer pool.Release()

	appendFn := func(offset uint32) func() {
		return func() {
			defer wg.Done()
			txn, _ := tae.StartTxn(nil)
			database, _ := txn.GetDatabase("db")
			rel, _ := database.GetRelationByName(schema.Name)
			err := rel.BatchDedup(bats[offset].Vecs[3])
			txn.Commit(context.Background())
			if err != nil {
				logutil.Infof("err is %v", err)
				return
			}

			txn2, _ := tae.StartTxn(nil)
			txn2.SetDedupType(txnif.IncrementalDedup)
			txn2.SetSnapshotTS(txn.GetStartTS())
			database, _ = txn2.GetDatabase("db")
			rel, _ = database.GetRelationByName(schema.Name)
			_ = rel.Append(context.Background(), bats[offset])
			_ = txn2.Commit(context.Background())
		}
	}

	for i := 0; i < totalRows; i++ {
		for j := 0; j < 5; j++ {
			wg.Add(1)
			err := pool.Submit(appendFn(uint32(i)))
			assert.Nil(t, err)
		}
	}
	wg.Wait()

	tae.CheckRowsByScan(totalRows, false)
}

func TestDeduplication(t *testing.T) {
	ctx := context.Background()
	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()

	schema := catalog.MockSchemaAll(60, 3)
	schema.BlockMaxRows = 2
	schema.SegmentMaxBlocks = 10
	tae.BindSchema(schema)
	testutil.CreateRelation(t, tae.DB, "db", schema, true)

	rows := 10
	bat := catalog.MockBatch(schema, rows)
	bats := bat.Split(rows)

	segmentIDs := make([]*types.Uuid, 2)
	segmentIDs[0] = objectio.NewSegmentid()
	segmentIDs[1] = objectio.NewSegmentid()
	sort.Slice(segmentIDs, func(i, j int) bool {
		return segmentIDs[i].Le(*segmentIDs[j])
	})

	blk1Name := objectio.BuildObjectName(segmentIDs[1], 0)
	writer, err := blockio.NewBlockWriterNew(tae.Runtime.Fs.Service, blk1Name, 0, nil)
	assert.NoError(t, err)
	writer.SetPrimaryKey(3)
	writer.WriteBatch(containers.ToCNBatch(bats[0]))
	blocks, _, err := writer.Sync(context.TODO())
	assert.NoError(t, err)
	assert.Equal(t, 1, len(blocks))

	metaLoc := blockio.EncodeLocation(
		writer.GetName(),
		blocks[0].GetExtent(),
		uint32(bats[0].Length()),
		blocks[0].GetID(),
	)

	txn, rel := tae.GetRelation()
	err = rel.AddBlksWithMetaLoc(context.Background(), []objectio.Location{metaLoc})
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	db, err := tae.Catalog.TxnGetDBEntryByName("db", txn)
	assert.NoError(t, err)
	tbl, err := db.TxnGetTableEntryByName(schema.Name, txn)
	assert.NoError(t, err)
	dataFactory := tables.NewDataFactory(
		tae.Runtime,
		tae.Dir)
	seg, err := tbl.CreateSegment(
		txn,
		catalog.ES_Appendable,
		dataFactory.MakeSegmentFactory(),
		new(objectio.CreateSegOpt).WithId(segmentIDs[0]))
	assert.NoError(t, err)
	blk, err := seg.CreateBlock(txn, catalog.ES_Appendable, dataFactory.MakeBlockFactory(), nil)
	assert.NoError(t, err)
	txn.GetStore().AddTxnEntry(txnif.TxnType_Normal, seg)
	txn.GetStore().IncreateWriteCnt()
	assert.NoError(t, txn.Commit(context.Background()))
	assert.NoError(t, blk.PrepareCommit())
	assert.NoError(t, blk.ApplyCommit())
	assert.NoError(t, seg.PrepareCommit())
	assert.NoError(t, seg.ApplyCommit())

	txns := make([]txnif.AsyncTxn, 0)
	for i := 0; i < 5; i++ {
		for j := 1; j < rows; j++ {
			txn, _ := tae.StartTxn(nil)
			database, _ := txn.GetDatabase("db")
			rel, _ = database.GetRelationByName(schema.Name)
			_ = rel.Append(context.Background(), bats[j])
			txns = append(txns, txn)
		}
	}
	for _, txn := range txns {
		txn.Commit(context.Background())
	}
	tae.CheckRowsByScan(rows, false)
	t.Logf(tae.Catalog.SimplePPString(3))
}

func TestGCInMemeoryDeletesByTS(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	rows := 100
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = uint32(rows)
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, rows)
	tae.CreateRelAndAppend(bat, true)

	txn, rel := tae.GetRelation()
	blkit := rel.MakeBlockIt()
	blkHandle := blkit.GetBlock()
	blkMeta := blkHandle.GetMeta().(*catalog.BlockEntry)
	blkID := blkMeta.AsCommonID()
	blkData := blkMeta.GetBlockData()
	assert.NoError(t, txn.Commit(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-ctx.Done():
				return
			default:
				ts := tae.TxnMgr.StatMaxCommitTS()
				batch, err := blkData.CollectDeleteInRange(context.Background(), types.TS{}, ts, true)
				assert.NoError(t, err)
				if batch == nil {
					continue
				}

				blk1Name := objectio.BuildObjectName(objectio.NewSegmentid(), uint16(i))
				writer, err := blockio.NewBlockWriterNew(tae.Runtime.Fs.Service, blk1Name, 0, nil)
				assert.NoError(t, err)
				writer.SetPrimaryKey(3)
				writer.WriteBatch(containers.ToCNBatch(batch))
				blocks, _, err := writer.Sync(context.TODO())
				assert.NoError(t, err)
				assert.Equal(t, 1, len(blocks))

				deltaLoc := blockio.EncodeLocation(
					writer.GetName(),
					blocks[0].GetExtent(),
					uint32(batch.Length()),
					blocks[0].GetID(),
				)

				txn, rel := tae.GetRelation()
				blkit := rel.MakeBlockIt()
				blkHandle := blkit.GetBlock()
				err = blkHandle.UpdateDeltaLoc(deltaLoc)
				assert.NoError(t, err)
				assert.NoError(t, txn.Commit(context.Background()))

				blkData.GCInMemeoryDeletesByTS(ts)
			}
			i++
		}
	}()

	for offset := 0; offset < rows; offset++ {
		txn, rel := tae.GetRelation()
		assert.NoError(t, rel.RangeDelete(blkID, uint32(offset), uint32(offset), handle.DT_Normal))
		assert.NoError(t, txn.Commit(context.Background()))
		ts := txn.GetCommitTS()

		batch, err := blkData.CollectDeleteInRange(context.Background(), types.TS{}, ts, true)
		assert.NoError(t, err)
		t.Logf(logtail.BatchToString("", batch, false))
		for i, vec := range batch.Vecs {
			t.Logf(batch.Attrs[i])
			assert.Equal(t, offset+1, vec.Length())
		}
		view, err := blkData.CollectChangesInRange(context.Background(), types.TS{}, ts)
		assert.NoError(t, err)
		t.Logf(view.DeleteMask.String())
		assert.Equal(t, offset+1, view.DeleteMask.GetCardinality())
	}
	cancel()
	wg.Wait()
}

func TestRW(t *testing.T) {
	ctx := context.Background()
	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	rows := 10
	schema := catalog.MockSchemaAll(5, 2)
	schema.BlockMaxRows = uint32(rows)
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, rows)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)

	txn1, rel1 := tae.GetRelation()
	v := bat.Vecs[2].Get(2)
	filter := handle.NewEQFilter(v)
	id, row, err := rel1.GetByFilter(ctx, filter)
	assert.NoError(t, err)
	err = rel1.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)

	meta := rel1.GetMeta().(*catalog.TableEntry)

	cnt := 3
	for i := 0; i < cnt; i++ {
		txn2, rel2 := tae.GetRelation()
		v = bat.Vecs[2].Get(i + 3)
		filter = handle.NewEQFilter(v)
		id, row, err = rel2.GetByFilter(ctx, filter)
		assert.NoError(t, err)
		err = rel2.RangeDelete(id, row, row, handle.DT_Normal)
		assert.NoError(t, err)
		err = txn2.Commit(ctx)
		assert.NoError(t, err)

		err = tae.FlushTable(
			ctx, 0, meta.GetDB().ID, meta.ID,
			types.BuildTS(time.Now().UTC().UnixNano(), 0),
		)
		assert.NoError(t, err)
	}

	err = txn1.Commit(ctx)
	assert.NoError(t, err)

	{
		txn, rel := tae.GetRelation()
		rcnt := testutil.GetColumnRowsByScan(t, rel, 2, true)
		assert.Equal(t, rows-cnt-1, rcnt)
		assert.NoError(t, txn.Commit(ctx))
	}
}

func TestReplayDeletes(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	rows := 250
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = 50
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, rows)
	defer bat.Close()
	bats := bat.Split(5)
	tae.CreateRelAndAppend(bats[0], true)
	//nablk
	tae.CompactBlocks(false)
	//deletes
	txn, rel := tae.GetRelation()
	blkIt := rel.MakeBlockIt()
	blk := blkIt.GetBlock()
	blk.RangeDelete(1, 50, handle.DT_Normal)
	assert.NoError(t, txn.Commit(context.Background()))
	//the next blk to compact
	tae.DoAppend(bats[1])
	//keep the segment appendable
	tae.DoAppend(bats[2])
	//compact nablk and its next blk
	txn2, rel := tae.GetRelation()
	blkIt = rel.MakeBlockIt()
	blkEntry := blkIt.GetBlock().GetMeta().(*catalog.BlockEntry)
	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	task, err := jobs.NewCompactBlockTask(nil, txn, blkEntry, tae.Runtime)
	assert.NoError(t, err)
	err = task.OnExec(context.Background())
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
	assert.NoError(t, txn2.Commit(context.Background()))
	//replay and check nextObjectIdx
	t.Log(tae.Catalog.SimplePPString(3))
	db, err := tae.Catalog.GetDatabaseByID(blkEntry.AsCommonID().DbID)
	assert.NoError(t, err)
	tbl, err := db.GetTableEntryByID(blkEntry.AsCommonID().TableID)
	assert.NoError(t, err)
	seg, err := tbl.GetSegmentByID(blkEntry.AsCommonID().SegmentID())
	segString1 := seg.Repr()
	assert.NoError(t, err)
	tae.Restart(context.Background())
	t.Log(tae.Catalog.SimplePPString(3))
	db, err = tae.Catalog.GetDatabaseByID(blkEntry.AsCommonID().DbID)
	assert.NoError(t, err)
	tbl, err = db.GetTableEntryByID(blkEntry.AsCommonID().TableID)
	assert.NoError(t, err)
	seg, err = tbl.GetSegmentByID(blkEntry.AsCommonID().SegmentID())
	assert.NoError(t, err)
	segString2 := seg.Repr()
	assert.Equal(t, segString1, segString2)
}
func TestApplyDeltalocation1(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	rows := 10
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = 10
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, rows)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)

	// apply deleteloc fails on ablk
	v1 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(1)
	ok, err := tae.TryDeleteByDeltaloc([]any{v1})
	assert.NoError(t, err)
	assert.False(t, ok)

	tae.CompactBlocks(false)
	filter := handle.NewEQFilter(v1)
	txn, rel := tae.GetRelation()
	id, offset, err := rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	ok, err = tae.TryDeleteByDeltaloc([]any{v1})
	assert.NoError(t, err)
	assert.True(t, ok)

	// range delete conflicts with deletes in deltaloc
	err = rel.RangeDelete(id, offset, offset, handle.DT_Normal)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	// apply deltaloc fails if there're persisted deletes
	v2 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(2)
	ok, err = tae.TryDeleteByDeltaloc([]any{v2})
	assert.NoError(t, err)
	assert.False(t, ok)

	// apply deltaloc fails if there're deletes in memory
	tae.CompactBlocks(false)
	v3 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter = handle.NewEQFilter(v3)
	txn, rel = tae.GetRelation()
	id, offset, err = rel.GetByFilter(context.Background(), filter)
	assert.NoError(t, err)
	err = rel.RangeDelete(id, offset, offset, handle.DT_Normal)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	v4 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(4)
	ok, err = tae.TryDeleteByDeltaloc([]any{v4})
	assert.NoError(t, err)
	assert.False(t, ok)

}

// test compact
func TestApplyDeltalocation2(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	rows := 10
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = 10
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, rows)
	bats := bat.Split(10)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)
	tae.CompactBlocks(false)

	v3 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	v5 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(5)
	filter3 := handle.NewEQFilter(v3)
	filter5 := handle.NewEQFilter(v5)

	// test logtail
	tae.LogtailMgr.RegisterCallback(logtail.MockCallback)
	tae.TryDeleteByDeltaloc([]any{v3, v5})
	t.Log(tae.Catalog.SimplePPString(3))

	txn, rel := tae.GetRelation()
	_, _, err := rel.GetByFilter(context.Background(), filter5)
	assert.Error(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter3)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
	tae.CheckRowsByScan(8, true)

	tae.Restart(context.Background())
	txn, rel = tae.GetRelation()
	_, _, err = rel.GetByFilter(context.Background(), filter5)
	assert.Error(t, err)
	_, _, err = rel.GetByFilter(context.Background(), filter3)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
	tae.CheckRowsByScan(8, true)

	// test dedup
	tae.DoAppend(bats[3])
	tae.CheckRowsByScan(9, true)

	// test compact
	tae.CompactBlocks(false)
	txn, rel = tae.GetRelation()
	_, _, err = rel.GetByFilter(context.Background(), filter5)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
	tae.CheckRowsByScan(9, true)

	tae.Restart(context.Background())
	txn, rel = tae.GetRelation()
	_, _, err = rel.GetByFilter(context.Background(), filter5)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
	tae.CheckRowsByScan(9, true)
}

func TestApplyDeltalocation3(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	rows := 10
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = 10
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, rows)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)
	tae.CompactBlocks(false)

	v3 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter3 := handle.NewEQFilter(v3)

	// apply deltaloc failed if there're new deletes

	v5 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(5)
	filter5 := handle.NewEQFilter(v5)
	txn, err := tae.StartTxn(nil)
	assert.NoError(t, err)
	ok, err := tae.TryDeleteByDeltalocWithTxn([]any{v3}, txn)
	assert.NoError(t, err)
	assert.True(t, ok)

	{
		// delete v5
		txn2, rel2 := tae.GetRelation()
		err = rel2.DeleteByFilter(context.Background(), filter5)
		assert.NoError(t, err)
		assert.NoError(t, txn2.Commit(context.Background()))
	}
	tae.CheckRowsByScan(9, true)

	assert.Error(t, txn.Commit(context.Background()))
	tae.CheckRowsByScan(9, true)

	// apply deltaloc successfully if txn of new deletes are active

	tae.CompactBlocks(false)
	txn, err = tae.StartTxn(nil)
	assert.NoError(t, err)
	ok, err = tae.TryDeleteByDeltalocWithTxn([]any{v3}, txn)
	assert.NoError(t, err)
	assert.True(t, ok)

	// delete v5
	v4 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(4)
	filter4 := handle.NewEQFilter(v4)
	txn2, rel2 := tae.GetRelation()
	err = rel2.DeleteByFilter(context.Background(), filter4)
	assert.NoError(t, err)

	assert.NoError(t, txn.Commit(context.Background()))
	tae.CheckRowsByScan(8, true)

	assert.NoError(t, txn2.Commit(context.Background()))
	tae.CheckRowsByScan(7, true)

	txn, rel := tae.GetRelation()
	_, _, err = rel.GetByFilter(context.Background(), filter3)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

}

func TestReplayPersistedDelete(t *testing.T) {
	defer testutils.AfterTest(t)()
	ctx := context.Background()

	opts := config.WithLongScanAndCKPOpts(nil)
	tae := testutil.NewTestEngine(ctx, ModuleName, t, opts)
	defer tae.Close()
	rows := 10
	schema := catalog.MockSchemaAll(2, 1)
	schema.BlockMaxRows = 10
	tae.BindSchema(schema)
	bat := catalog.MockBatch(schema, rows)
	defer bat.Close()
	tae.CreateRelAndAppend(bat, true)
	tae.CompactBlocks(false)

	v3 := bat.Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter3 := handle.NewEQFilter(v3)
	txn, rel := tae.GetRelation()
	id, offset, err := rel.GetByFilter(context.Background(), filter3)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit(context.Background()))

	ok, err := tae.TryDeleteByDeltaloc([]any{v3})
	assert.NoError(t, err)
	assert.True(t, ok)

	tae.Restart(context.Background())

	txn, rel = tae.GetRelation()
	err = rel.RangeDelete(id, offset, offset, handle.DT_Normal)
	assert.Error(t, err)
	assert.NoError(t, txn.Commit(context.Background()))
}