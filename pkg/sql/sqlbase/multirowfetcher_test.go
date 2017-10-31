// Copyright 2017 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sqlbase

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"golang.org/x/net/context"
)

const testDB = "test"

type initFetcherArgs struct {
	tableDesc       *TableDescriptor
	indexIdx        int
	valNeededForCol []bool
}

func initFetcher(
	entries []initFetcherArgs, reverseScan bool, alloc *DatumAlloc,
) (fetcher *MultiRowFetcher, err error) {
	fetcher = &MultiRowFetcher{}

	fetcherArgs := make([]MultiRowFetcherTableArgs, len(entries))

	for i, entry := range entries {
		var index *IndexDescriptor
		var isSecondaryIndex bool

		if entry.indexIdx > 0 {
			index = &entry.tableDesc.Indexes[entry.indexIdx-1]
			isSecondaryIndex = true
		} else {
			index = &entry.tableDesc.PrimaryIndex
		}

		colIdxMap := make(map[ColumnID]int)
		for i, c := range entry.tableDesc.Columns {
			colIdxMap[c.ID] = i
		}

		fetcherArgs[i] = MultiRowFetcherTableArgs{
			Desc:             entry.tableDesc,
			Index:            index,
			ColIdxMap:        colIdxMap,
			IsSecondaryIndex: isSecondaryIndex,
			Cols:             entry.tableDesc.Columns,
			ValNeededForCol:  entry.valNeededForCol,
		}
	}

	if err := fetcher.Init(fetcherArgs, reverseScan, false /*reverse*/, false /*lockForUpdate*/, alloc); err != nil {
		return nil, err
	}

	return fetcher, nil
}

type fetcherEntryArgs struct {
	tableName        string
	indexName        string // Specify if this entry is an index
	indexIdx         int    // 0 for primary index (default)
	modFactor        int    // Useful modulo to apply for value columns
	schema           string
	interleaveSchema string // Specify if this entry is to be interleaved into another table
	indexSchema      string // Specify if this entry is to be created as an index
	nRows            int
	nCols            int // Number of columns in the table
	nVals            int // Number of values requested from scan
	valNeededForCol  []bool
	genValue         sqlutils.GenRowFn
}

func TestNextRowSingle(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	tables := map[string]fetcherEntryArgs{
		"t1": {
			modFactor: 42,
			nRows:     1337,
			nCols:     2,
		},
		"t2": {
			modFactor: 13,
			nRows:     2014,
			nCols:     2,
		},
		"norows": {
			modFactor: 10,
			nRows:     0,
			nCols:     2,
		},
		"onerow": {
			modFactor: 10,
			nRows:     1,
			nCols:     2,
		},
	}

	// Initialize tables first.
	var maxCols int
	for tableName, table := range tables {
		sqlutils.CreateTable(
			t, sqlDB, tableName,
			"k INT PRIMARY KEY, v INT",
			table.nRows,
			sqlutils.ToRowFn(sqlutils.RowIdxFn, sqlutils.RowModuloFn(table.modFactor)),
		)
		if table.nCols > maxCols {
			maxCols = table.nCols
		}
	}

	alloc := &DatumAlloc{}

	// Backing slice for specifying which column values should be returned.
	// By default, we want all the columns.
	valNeededForCol := make([]bool, maxCols)
	for i := range valNeededForCol {
		valNeededForCol[i] = true
	}

	// We try to read rows from each table.
	for tableName, table := range tables {
		t.Run(tableName, func(t *testing.T) {
			tableDesc := GetTableDescriptor(kvDB, testDB, tableName)
			args := []initFetcherArgs{
				{
					tableDesc:       tableDesc,
					indexIdx:        0,
					valNeededForCol: valNeededForCol[:table.nCols],
				},
			}

			mrf, err := initFetcher(args, false /*reverseScan*/, alloc)
			if err != nil {
				t.Fatal(err)
			}

			if err := mrf.StartScan(
				context.TODO(),
				client.NewTxn(kvDB, 0),
				roachpb.Spans{tableDesc.TableSpan()},
				false, /*limitBatches*/
				0,     /*limitHint*/
				false, /*traceKV*/
			); err != nil {
				t.Fatal(err)
			}

			count := 0

			expectedVals := [2]int64{1, 1}
			for {
				resp, err := mrf.NextRowDecoded(context.TODO(), false /*traceKV*/)
				if err != nil {
					t.Fatal(err)
				}
				if resp.Datums == nil {
					break
				}

				count++

				if resp.Desc.ID != tableDesc.ID || resp.Index.ID != tableDesc.PrimaryIndex.ID {
					t.Fatalf(
						"unexpected row retrieved from fetcher.\nnexpected:  table %s - index %s\nactual: table %s - index %s",
						tableDesc.Name, tableDesc.PrimaryIndex.Name,
						resp.Desc.Name, resp.Index.Name,
					)
				}

				if table.nCols != len(resp.Datums) {
					t.Fatalf("expected %d columns, got %d columns", table.nCols, len(resp.Datums))
				}

				for i, expected := range expectedVals {
					actual := int64(*resp.Datums[i].(*parser.DInt))
					if expected != actual {
						t.Fatalf("unexpected value for row %d, col %d.\nexpected: %d\nactual: %d", count, i, expected, actual)
					}
				}

				expectedVals[0]++
				expectedVals[1]++
				// Value column is in terms of a modulo.
				expectedVals[1] %= int64(table.modFactor)
			}

			if table.nRows != count {
				t.Fatalf("expected %d rows, got %d rows", table.nRows, count)
			}
		})
	}
}

// Secondary indexes contain extra values (the primary key of the primary index
// as well as STORING columns).
func TestNextRowSecondaryIndex(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	// Modulo to use for s1, s2 storing columns.
	storingMods := [2]int{7, 13}
	// Number of NULL secondary index values.
	nNulls := 20

	tables := map[string]*fetcherEntryArgs{
		"nonunique": {
			modFactor:       20,
			schema:          "p INT PRIMARY KEY, idx INT, s1 INT, s2 INT, INDEX i1 (idx)",
			nRows:           422,
			nCols:           4,
			nVals:           2,
			valNeededForCol: []bool{true, true, false, false},
		},
		"unique": {
			// Must be > nRows since this value must be unique.
			modFactor:       1000,
			schema:          "p INT PRIMARY KEY, idx INT, s1 INT, s2 INT, UNIQUE INDEX i1 (idx)",
			nRows:           123,
			nCols:           4,
			nVals:           2,
			valNeededForCol: []bool{true, true, false, false},
		},
		"nonuniquestoring": {
			modFactor: 42,
			schema:    "p INT PRIMARY KEY, idx INT, s1 INT, s2 INT, INDEX i1 (idx) STORING (s1, s2)",
			nRows:     654,
			nCols:     4,
			nVals:     4,
		},
		"uniquestoring": {
			// Must be > nRows since this value must be unique.
			modFactor: 1000,
			nRows:     555,
			schema:    "p INT PRIMARY KEY, idx INT, s1 INT, s2 INT, UNIQUE INDEX i1 (idx) STORING (s1, s2)",
			nCols:     4,
			nVals:     4,
		},
	}

	// Initialize the generate value functions.
	tables["nonunique"].genValue = sqlutils.ToRowFn(
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tables["nonunique"].modFactor),
	)
	tables["unique"].genValue = sqlutils.ToRowFn(
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tables["unique"].modFactor),
	)
	tables["nonuniquestoring"].genValue = sqlutils.ToRowFn(
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tables["nonuniquestoring"].modFactor),
		sqlutils.RowModuloFn(storingMods[0]),
		sqlutils.RowModuloFn(storingMods[1]),
	)
	tables["uniquestoring"].genValue = sqlutils.ToRowFn(
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tables["uniquestoring"].modFactor),
		sqlutils.RowModuloFn(storingMods[0]),
		sqlutils.RowModuloFn(storingMods[1]),
	)

	r := sqlutils.MakeSQLRunner(t, sqlDB)
	// Initialize tables first.
	var maxCols int
	for tableName, table := range tables {
		sqlutils.CreateTable(
			t, sqlDB, tableName,
			table.schema,
			table.nRows,
			table.genValue,
		)
		if table.nCols > maxCols {
			maxCols = table.nCols
		}

		// Insert nNulls NULL secondary index values (this tests if
		// we're properly decoding (UNIQUE) secondary index keys
		// properly).
		for i := 1; i <= nNulls; i++ {
			r.Exec(fmt.Sprintf(
				`INSERT INTO test.%s VALUES (%d, NULL, %d, %d);`,
				tableName,
				table.nRows+i,
				(table.nRows+i)%storingMods[0],
				(table.nRows+i)%storingMods[1],
			))
		}
		table.nRows += nNulls
	}

	alloc := &DatumAlloc{}

	// Backing slice for specifying which column values should be returned.
	// By default, we want all the columns.
	valNeededForCol := make([]bool, maxCols)
	for i := range valNeededForCol {
		valNeededForCol[i] = true
	}

	// We try to read rows from each index.
	for tableName, table := range tables {
		t.Run(tableName, func(t *testing.T) {
			tableDesc := GetTableDescriptor(kvDB, testDB, tableName)

			colsNeeded := valNeededForCol[:table.nCols]
			if table.valNeededForCol != nil {
				colsNeeded = table.valNeededForCol
			}
			args := []initFetcherArgs{
				{
					tableDesc: tableDesc,
					// We scan from the first secondary index.
					indexIdx:        1,
					valNeededForCol: colsNeeded,
				},
			}

			mrf, err := initFetcher(args, false /*reverseScan*/, alloc)
			if err != nil {
				t.Fatal(err)
			}

			if err := mrf.StartScan(
				context.TODO(),
				client.NewTxn(kvDB, 0),
				roachpb.Spans{tableDesc.TableSpan()},
				false, /*limitBatches*/
				0,     /*limitHint*/
				false, /*traceKV*/
			); err != nil {
				t.Fatal(err)
			}

			count := 0
			nullCount := 0
			var prevIdxVal int64
			for {
				resp, err := mrf.NextRowDecoded(context.TODO(), false /*traceKV*/)
				if err != nil {
					t.Fatal(err)
				}
				if resp.Datums == nil {
					break
				}

				count++

				if resp.Desc.ID != tableDesc.ID || resp.Index.ID != tableDesc.Indexes[0].ID {
					t.Fatalf(
						"unexpected row retrieved from fetcher.\nnexpected:  table %s - index %s\nactual: table %s - index %s",
						tableDesc.Name, tableDesc.Indexes[0].Name,
						resp.Desc.Name, resp.Index.Name,
					)
				}

				if table.nCols != len(resp.Datums) {
					t.Fatalf("expected %d columns, got %d columns", table.nCols, len(resp.Datums))
				}

				// Verify that the correct # of values are returned.
				numVals := 0
				for _, datum := range resp.Datums {
					if datum != parser.DNull {
						numVals++
					}
				}

				// Some secondary index values can be NULL. We keep track
				// of how many we encounter.
				idxNull := resp.Datums[1] == parser.DNull
				if idxNull {
					nullCount++
					// It is okay to bump this up by one since we know
					// this index value is suppose to be NULL.
					numVals++
				}

				if table.nVals != numVals {
					t.Fatalf("expected %d non-NULL values, got %d", table.nVals, numVals)
				}

				id := int64(*resp.Datums[0].(*parser.DInt))
				// Verify the value in the value column is
				// correct (if it is not NULL).
				if !idxNull {
					idx := int64(*resp.Datums[1].(*parser.DInt))
					if id%int64(table.modFactor) != idx {
						t.Fatalf("for row id %d, expected %d value, got %d", id, id%int64(table.modFactor), idx)
					}

					// Index values must be fetched in
					// non-decreasing order.
					if prevIdxVal > idx {
						t.Fatalf("index value unexpectedly decreased from %d to %d", prevIdxVal, idx)
					}
					prevIdxVal = idx
				}

				// We verify that the storing values are
				// decoded correctly.
				if tableName == "nonuniquestoring" || tableName == "uniquestoring" {
					s1 := int64(*resp.Datums[2].(*parser.DInt))
					s2 := int64(*resp.Datums[3].(*parser.DInt))

					if id%int64(storingMods[0]) != s1 {
						t.Fatalf("for row id %d, expected %d for s1 value, got %d", id, id%int64(storingMods[0]), s1)
					}
					if id%int64(storingMods[1]) != s2 {
						t.Fatalf("for row id %d, expected %d for s2 value, got %d", id, id%int64(storingMods[1]), s2)
					}
				}
			}

			if table.nRows != count {
				t.Fatalf("expected %d rows, got %d rows", table.nRows, count)
			}
		})
	}
}

// Appends all non-empty subsets of indices in [0, maxIdx).
func generateIdxSubsets(maxIdx int, subsets [][]int) [][]int {
	if maxIdx < 0 {
		return subsets
	}
	subsets = generateIdxSubsets(maxIdx-1, subsets)
	curLength := len(subsets)
	for i := 0; i < curLength; i++ {
		// Keep original subsets by duplicating them.
		dupe := make([]int, len(subsets[i]))
		copy(dupe, subsets[i])
		subsets = append(subsets, dupe)
		// Generate new subsets with the current index.
		subsets[i] = append(subsets[i], maxIdx)
	}
	return append(subsets, []int{maxIdx})
}

// We test reading rows from six tables in a database that contains two
// interleave hierarchies.
// The tables are structured as follows:
// parent1
// parent2
//   child1
//      grandchild1
//	  grandgrandchild1
//   child2
//   grandgrandchild1@ggc1_unique_idx
// parent3
// We test reading rows from every non-empty subset for completeness.
func TestNextRowInterleave(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	tableArgs := map[string]*fetcherEntryArgs{
		"parent1": {
			tableName: "parent1",
			modFactor: 12,
			schema:    "p1 INT PRIMARY KEY, v INT",
			nRows:     100,
			nCols:     2,
			nVals:     2,
		},
		"parent2": {
			tableName: "parent2",
			modFactor: 3,
			schema:    "p2 INT PRIMARY KEY, v INT",
			nRows:     400,
			nCols:     2,
			nVals:     2,
		},
		"child1": {
			tableName:        "child1",
			modFactor:        5,
			schema:           "p2 INT, c1 INT, v INT, PRIMARY KEY (p2, c1)",
			interleaveSchema: "test.parent2 (p2)",
			// child1 has more rows than parent2, thus some parent2
			// rows will have multiple child1.
			nRows: 500,
			nCols: 3,
			nVals: 3,
		},
		"grandchild1": {
			tableName:        "grandchild1",
			modFactor:        7,
			schema:           "p2 INT, c1 INT, gc1 INT, v INT, PRIMARY KEY (p2, c1, gc1)",
			interleaveSchema: "test.child1 (p2, c1)",
			nRows:            2000,
			nCols:            4,
			nVals:            4,
		},
		"grandgrandchild1": {
			tableName:        "grandgrandchild1",
			modFactor:        12,
			schema:           "p2 INT, c1 INT, gc1 INT, ggc1 INT, v INT, PRIMARY KEY (p2, c1, gc1, ggc1)",
			interleaveSchema: "test.grandchild1 (p2, c1, gc1)",
			nRows:            350,
			nCols:            5,
			nVals:            5,
		},
		"child2": {
			tableName:        "child2",
			modFactor:        42,
			schema:           "p2 INT, c2 INT, v INT, PRIMARY KEY (p2, c2)",
			interleaveSchema: "test.parent2 (p2)",
			// child2 has less rows than parent2, thus not all
			// parent2 rows will have a nested child2 row.
			nRows: 100,
			nCols: 3,
			nVals: 3,
		},
		"parent3": {
			tableName: "parent3",
			modFactor: 42,
			schema:    "p3 INT PRIMARY KEY, v INT",
			nRows:     1,
			nCols:     2,
			nVals:     2,
		},
	}

	// Initialize generating value functions for each table.
	tableArgs["parent1"].genValue = sqlutils.ToRowFn(
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tableArgs["parent1"].modFactor),
	)

	tableArgs["parent2"].genValue = sqlutils.ToRowFn(
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tableArgs["parent2"].modFactor),
	)

	tableArgs["child1"].genValue = sqlutils.ToRowFn(
		// Foreign key needs a shifted modulo.
		sqlutils.RowModuloShiftedFn(tableArgs["parent2"].nRows),
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tableArgs["child1"].modFactor),
	)

	tableArgs["grandchild1"].genValue = sqlutils.ToRowFn(
		// Foreign keys need a shifted modulo.
		sqlutils.RowModuloShiftedFn(
			tableArgs["child1"].nRows,
			tableArgs["parent2"].nRows,
		),
		sqlutils.RowModuloShiftedFn(tableArgs["child1"].nRows),
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tableArgs["grandchild1"].modFactor),
	)

	tableArgs["grandgrandchild1"].genValue = sqlutils.ToRowFn(
		// Foreign keys need a shifted modulo.
		sqlutils.RowModuloShiftedFn(
			tableArgs["grandchild1"].nRows,
			tableArgs["child1"].nRows,
			tableArgs["parent2"].nRows,
		),
		sqlutils.RowModuloShiftedFn(
			tableArgs["grandchild1"].nRows,
			tableArgs["child1"].nRows,
		),
		sqlutils.RowModuloShiftedFn(tableArgs["grandchild1"].nRows),
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tableArgs["grandgrandchild1"].modFactor),
	)

	tableArgs["child2"].genValue = sqlutils.ToRowFn(
		// Foreign key needs a shifted modulo.
		sqlutils.RowModuloShiftedFn(tableArgs["parent2"].nRows),
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tableArgs["child2"].modFactor),
	)

	tableArgs["parent3"].genValue = sqlutils.ToRowFn(
		sqlutils.RowIdxFn,
		sqlutils.RowModuloFn(tableArgs["parent3"].modFactor),
	)

	ggc1idx := *tableArgs["grandgrandchild1"]
	// This is only possible since nrows(ggc1) < nrows(p2) thus c1 is
	// unique.
	ggc1idx.indexSchema = `CREATE UNIQUE INDEX ggc1_unique_idx ON test.grandgrandchild1 (p2) INTERLEAVE IN PARENT test.parent2 (p2);`
	ggc1idx.indexName = "ggc1_unique_idx"
	ggc1idx.indexIdx = 1
	// Last column v is not stored in this index.
	ggc1idx.valNeededForCol = []bool{true, true, true, true, false}
	ggc1idx.nVals = 4

	// We need an ordering of the tables in order to execute the interleave
	// DDL statements.
	interleaveEntries := []fetcherEntryArgs{
		*tableArgs["parent1"],
		*tableArgs["parent2"],
		*tableArgs["child1"],
		*tableArgs["grandchild1"],
		*tableArgs["grandgrandchild1"],
		*tableArgs["child2"],
		ggc1idx,
		*tableArgs["parent3"],
	}

	var maxCols int
	for _, table := range interleaveEntries {
		if table.indexSchema != "" {
			// Create interleaved secondary indexes.
			r := sqlutils.MakeSQLRunner(t, sqlDB)
			r.Exec(table.indexSchema)
		} else {
			// Create tables (primary indexes).
			sqlutils.CreateTableInterleave(
				t, sqlDB, table.tableName,
				table.schema,
				table.interleaveSchema,
				table.nRows,
				table.genValue,
			)
		}

		if table.nCols > maxCols {
			maxCols = table.nCols
		}

	}

	alloc := &DatumAlloc{}
	// Backing slice for specifying which column values should be returned.
	// By default, we want all the columns.
	valNeededForCol := make([]bool, maxCols)
	for i := range valNeededForCol {
		valNeededForCol[i] = true
	}

	// Retrieve rows from every non-empty subset of the tables/indexes.
	for _, idxs := range generateIdxSubsets(len(interleaveEntries)-1, nil) {
		// Initialize our subset of tables/indexes.
		entries := make([]*fetcherEntryArgs, len(idxs))
		testNames := make([]string, len(entries))
		for i, idx := range idxs {
			entries[i] = &interleaveEntries[idx]
			testNames[i] = entries[i].tableName
			// Use the index name instead if we're scanning an index.
			if entries[i].indexName != "" {
				testNames[i] = entries[i].indexName
			}
		}

		testName := strings.Join(testNames, "-")

		t.Run(testName, func(t *testing.T) {
			// Initialize the MultiRowFetcher.
			args := make([]initFetcherArgs, len(entries))
			lookupSpans := make([]roachpb.Span, len(entries))
			// Used during NextRow to see if tableID << 32 |
			// indexID (key) are with what we initialize
			// MultiRowFetcher.
			idLookups := make(map[uint64]*fetcherEntryArgs, len(entries))
			for i, entry := range entries {
				tableDesc := GetTableDescriptor(kvDB, testDB, entry.tableName)
				var indexID IndexID
				if entry.indexIdx == 0 {
					indexID = tableDesc.PrimaryIndex.ID
				} else {
					indexID = tableDesc.Indexes[entry.indexIdx-1].ID
				}
				idLookups[idLookupKey(tableDesc.ID, indexID)] = entry

				colsNeeded := valNeededForCol[:entry.nCols]
				if entry.valNeededForCol != nil {
					colsNeeded = entry.valNeededForCol
				}

				args[i] = initFetcherArgs{
					tableDesc:       tableDesc,
					indexIdx:        entry.indexIdx,
					valNeededForCol: colsNeeded,
				}

				// We take every entry's index span (primary or
				// secondary) and use it to start our scan.
				lookupSpans[i] = tableDesc.IndexSpan(indexID)
			}

			lookupSpans, _ = roachpb.MergeSpans(lookupSpans)

			mrf, err := initFetcher(args, false /*reverseScan*/, alloc)
			if err != nil {
				t.Fatal(err)
			}

			if err := mrf.StartScan(
				context.TODO(),
				client.NewTxn(kvDB, 0),
				lookupSpans,
				false, /*limitBatches*/
				0,     /*limitHint*/
				false, /*traceKV*/
			); err != nil {
				t.Fatal(err)
			}

			// Running count of rows processed for each table-index.
			count := make(map[string]int, len(entries))

			for {
				resp, err := mrf.NextRowDecoded(context.TODO(), false /*traceKV*/)
				if err != nil {
					t.Fatal(err)
				}
				if resp.Datums == nil {
					break
				}

				entry, found := idLookups[idLookupKey(resp.Desc.ID, resp.Index.ID)]
				if !found {
					t.Fatalf(
						"unexpected row from table %s - index %s",
						resp.Desc.Name, resp.Index.Name,
					)
				}

				tableIdxName := fmt.Sprintf("%s@%s", entry.tableName, entry.indexName)
				count[tableIdxName]++

				// Check that the correct # of columns is returned.
				if entry.nCols != len(resp.Datums) {
					t.Fatalf("for table %s expected %d columns, got %d columns", tableIdxName, entry.nCols, len(resp.Datums))
				}

				// Verify that the correct # of values are returned.
				numVals := 0
				for _, datum := range resp.Datums {
					if datum != parser.DNull {
						numVals++
					}
				}
				if entry.nVals != numVals {
					t.Fatalf("for table %s expected %d non-NULL values, got %d", tableIdxName, entry.nVals, numVals)
				}

				// Verify the value in the value column is
				// correct if it is requested.
				if entry.nVals == entry.nCols {
					id := int64(*resp.Datums[entry.nCols-2].(*parser.DInt))
					val := int64(*resp.Datums[entry.nCols-1].(*parser.DInt))

					if id%int64(entry.modFactor) != val {
						t.Fatalf("for table %s row id %d, expected %d value, got %d", tableIdxName, id, id%int64(entry.modFactor), val)
					}
				}
			}

			for tableIdxName, actual := range count {
				// tableIdxName is formatted as tableName@indexName.
				tableName := strings.Split(tableIdxName, "@")[0]
				if tableArgs[tableName].nRows != actual {
					t.Fatalf("for table %s expected %d rows, got %d rows", tableName, tableArgs[tableName].nRows, actual)
				}
			}
		})
	}
}

func idLookupKey(tableID ID, indexID IndexID) uint64 {
	return (uint64(tableID) << 32) | uint64(indexID)
}
