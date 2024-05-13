package logstorage

import (
	"container/heap"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/memory"
)

// pipeSort processes '| sort ...' queries.
//
// See https://docs.victoriametrics.com/victorialogs/logsql/#sort-pipe
type pipeSort struct {
	// byFields contains field names for sorting from 'by(...)' clause.
	byFields []*bySortField

	// whether to apply descending order
	isDesc bool
}

func (ps *pipeSort) String() string {
	s := "sort"
	if len(ps.byFields) > 0 {
		a := make([]string, len(ps.byFields))
		for i, bf := range ps.byFields {
			a[i] = bf.String()
		}
		s += " by (" + strings.Join(a, ", ") + ")"
	}
	if ps.isDesc {
		s += " desc"
	}
	return s
}

func (ps *pipeSort) updateNeededFields(neededFields, unneededFields fieldsSet) {
	if len(ps.byFields) == 0 {
		neededFields.add("*")
		unneededFields.reset()
	} else {
		for _, bf := range ps.byFields {
			neededFields.add(bf.name)
			unneededFields.remove(bf.name)
		}
	}
}

func (ps *pipeSort) newPipeProcessor(workersCount int, stopCh <-chan struct{}, cancel func(), ppBase pipeProcessor) pipeProcessor {
	maxStateSize := int64(float64(memory.Allowed()) * 0.2)

	shards := make([]pipeSortProcessorShard, workersCount)
	for i := range shards {
		shard := &shards[i]
		shard.ps = ps
		shard.stateSizeBudget = stateSizeBudgetChunk
		maxStateSize -= stateSizeBudgetChunk
	}

	psp := &pipeSortProcessor{
		ps:     ps,
		stopCh: stopCh,
		cancel: cancel,
		ppBase: ppBase,

		shards: shards,

		maxStateSize: maxStateSize,
	}
	psp.stateSizeBudget.Store(maxStateSize)

	return psp
}

type pipeSortProcessor struct {
	ps     *pipeSort
	stopCh <-chan struct{}
	cancel func()
	ppBase pipeProcessor

	shards []pipeSortProcessorShard

	maxStateSize    int64
	stateSizeBudget atomic.Int64
}

type pipeSortProcessorShard struct {
	pipeSortProcessorShardNopad

	// The padding prevents false sharing on widespread platforms with 128 mod (cache line size) = 0 .
	_ [128 - unsafe.Sizeof(pipeSortProcessorShardNopad{})%128]byte
}

type pipeSortProcessorShardNopad struct {
	// ps points to the parent pipeSort.
	ps *pipeSort

	// blocks holds all the blocks with logs written to the shard.
	blocks []sortBlock

	// rowRefs holds references to all the rows stored in blocks.
	//
	// Sorting sorts rowRefs, while blocks remain unchanged. This should speed up sorting.
	rowRefs []sortRowRef

	// rowRefNext points to the next index at rowRefs during merge shards phase
	rowRefNext int

	// stateSizeBudget is the remaining budget for the whole state size for the shard.
	// The per-shard budget is provided in chunks from the parent pipeSortProcessor.
	stateSizeBudget int
}

// sortBlock represents a block of logs for sorting.
type sortBlock struct {
	// br is a result block to sort
	br *blockResult

	// byColumns refers block data for 'by(...)' columns
	byColumns []sortBlockByColumn

	// otherColumns refers block data for other than 'by(...)' columns
	otherColumns []*blockResultColumn
}

// sortBlockByColumn represents data for a single column from 'sort by(...)' clause.
type sortBlockByColumn struct {
	// c contains column data
	c *blockResultColumn

	// i64Values contains int64 numbers parsed from values
	i64Values []int64

	// f64Values contains float64 numbers parsed from values
	f64Values []float64
}

// sortRowRef is the reference to a single log entry written to `sort` pipe.
type sortRowRef struct {
	// blockIdx is the index of the block at pipeSortProcessorShard.blocks.
	blockIdx int

	// rowIdx is the index of the log entry inside the block referenced by blockIdx.
	rowIdx int
}

func (c *sortBlockByColumn) getI64ValueAtRow(rowIdx int) int64 {
	if c.c.isConst {
		return c.i64Values[0]
	}
	return c.i64Values[rowIdx]
}

func (c *sortBlockByColumn) getF64ValueAtRow(rowIdx int) float64 {
	if c.c.isConst {
		return c.f64Values[0]
	}
	return c.f64Values[rowIdx]
}

// writeBlock writes br to shard.
func (shard *pipeSortProcessorShard) writeBlock(br *blockResult) {
	// clone br, so it could be owned by shard
	br = br.clone()
	cs := br.getColumns()

	byFields := shard.ps.byFields
	if len(byFields) == 0 {
		// Sort by all the columns

		// Generate byColumns
		var rc resultColumn
		bb := bbPool.Get()
		for i := range br.timestamps {
			// JSON-encode all the columns per each row into a single string
			// and sort rows by the resulting string.
			bb.B = bb.B[:0]
			for _, c := range cs {
				v := c.getValueAtRow(br, i)
				bb.B = marshalJSONKeyValue(bb.B, c.name, v)
				bb.B = append(bb.B, ',')
			}
			rc.addValue(bytesutil.ToUnsafeString(bb.B))
		}
		bbPool.Put(bb)

		i64Values := make([]int64, len(br.timestamps))
		f64Values := make([]float64, len(br.timestamps))
		for i := range f64Values {
			f64Values[i] = nan
		}
		byColumns := []sortBlockByColumn{
			{
				c: &blockResultColumn{
					valueType:     valueTypeString,
					encodedValues: rc.values,
				},
				i64Values: i64Values,
				f64Values: f64Values,
			},
		}
		shard.stateSizeBudget -= len(rc.buf) + int(unsafe.Sizeof(byColumns[0])+unsafe.Sizeof(*byColumns[0].c))

		// Append br to shard.blocks.
		shard.blocks = append(shard.blocks, sortBlock{
			br:           br,
			byColumns:    byColumns,
			otherColumns: cs,
		})
	} else {
		// Collect values for columns from byFields.
		byColumns := make([]sortBlockByColumn, len(byFields))
		for i, bf := range byFields {
			c := br.getColumnByName(bf.name)
			bc := &byColumns[i]
			bc.c = c

			if c.isTime {
				// Do not initialize bc.i64Values and bc.f64Values, since they aren't used.
				// This saves some memory.
				continue
			}
			if c.isConst {
				bc.i64Values = shard.createInt64Values(c.encodedValues)
				bc.f64Values = shard.createFloat64Values(c.encodedValues)
				continue
			}

			// pre-populate values in order to track better br memory usage
			values := c.getValues(br)
			bc.i64Values = shard.createInt64Values(values)
			bc.f64Values = shard.createFloat64Values(values)
		}
		shard.stateSizeBudget -= len(byColumns) * int(unsafe.Sizeof(byColumns[0]))

		// Collect values for other columns.
		otherColumns := make([]*blockResultColumn, 0, len(cs))
		for _, c := range cs {
			isByField := false
			for _, bf := range byFields {
				if bf.name == c.name {
					isByField = true
					break
				}
			}
			if !isByField {
				otherColumns = append(otherColumns, c)
			}
		}
		shard.stateSizeBudget -= len(otherColumns) * int(unsafe.Sizeof(otherColumns[0]))

		// Append br to shard.blocks.
		shard.blocks = append(shard.blocks, sortBlock{
			br:           br,
			byColumns:    byColumns,
			otherColumns: otherColumns,
		})
	}

	shard.stateSizeBudget -= br.sizeBytes()
	shard.stateSizeBudget -= int(unsafe.Sizeof(shard.blocks[0]))

	// Add row references to rowRefs.
	blockIdx := len(shard.blocks) - 1
	rowRefs := shard.rowRefs
	rowRefsLen := len(rowRefs)
	for i := range br.timestamps {
		rowRefs = append(rowRefs, sortRowRef{
			blockIdx: blockIdx,
			rowIdx:   i,
		})
	}
	shard.rowRefs = rowRefs
	shard.stateSizeBudget -= (len(rowRefs) - rowRefsLen) * int(unsafe.Sizeof(rowRefs[0]))
}

func (shard *pipeSortProcessorShard) createInt64Values(values []string) []int64 {
	a := make([]int64, len(values))
	for i, v := range values {
		i64, ok := tryParseInt64(v)
		if ok {
			a[i] = i64
			continue
		}
		u32, _ := tryParseIPv4(v)
		a[i] = int64(u32)
		// Do not try parsing timestamp and duration, since they may be negative.
		// This breaks sorting.
	}

	shard.stateSizeBudget -= len(a) * int(unsafe.Sizeof(a[0]))

	return a
}

func (shard *pipeSortProcessorShard) createFloat64Values(values []string) []float64 {
	a := make([]float64, len(values))
	for i, v := range values {
		f, ok := tryParseFloat64(v)
		if !ok {
			f = nan
		}
		a[i] = f
	}

	shard.stateSizeBudget -= len(a) * int(unsafe.Sizeof(a[0]))

	return a
}

func (shard *pipeSortProcessorShard) Len() int {
	return len(shard.rowRefs)
}

func (shard *pipeSortProcessorShard) Swap(i, j int) {
	rowRefs := shard.rowRefs
	rowRefs[i], rowRefs[j] = rowRefs[j], rowRefs[i]
}

func (shard *pipeSortProcessorShard) Less(i, j int) bool {
	return sortBlockLess(shard, i, shard, j)
}

func (psp *pipeSortProcessor) writeBlock(workerID uint, br *blockResult) {
	if len(br.timestamps) == 0 {
		return
	}

	shard := &psp.shards[workerID]

	for shard.stateSizeBudget < 0 {
		// steal some budget for the state size from the global budget.
		remaining := psp.stateSizeBudget.Add(-stateSizeBudgetChunk)
		if remaining < 0 {
			// The state size is too big. Stop processing data in order to avoid OOM crash.
			if remaining+stateSizeBudgetChunk >= 0 {
				// Notify worker goroutines to stop calling writeBlock() in order to save CPU time.
				psp.cancel()
			}
			return
		}
		shard.stateSizeBudget += stateSizeBudgetChunk
	}

	shard.writeBlock(br)
}

func (psp *pipeSortProcessor) flush() error {
	if n := psp.stateSizeBudget.Load(); n <= 0 {
		return fmt.Errorf("cannot calculate [%s], since it requires more than %dMB of memory", psp.ps.String(), psp.maxStateSize/(1<<20))
	}

	select {
	case <-psp.stopCh:
		return nil
	default:
	}

	// Sort every shard in parallel
	var wg sync.WaitGroup
	shards := psp.shards
	for i := range shards {
		wg.Add(1)
		go func(shard *pipeSortProcessorShard) {
			// TODO: interrupt long sorting when psp.stopCh is closed.
			sort.Sort(shard)
			wg.Done()
		}(&shards[i])
	}
	wg.Wait()

	select {
	case <-psp.stopCh:
		return nil
	default:
	}

	// Merge sorted results across shards
	sh := pipeSortProcessorShardsHeap(make([]*pipeSortProcessorShard, 0, len(shards)))
	for i := range shards {
		shard := &shards[i]
		if shard.Len() > 0 {
			sh = append(sh, shard)
		}
	}
	if len(sh) == 0 {
		return nil
	}

	heap.Init(&sh)

	wctx := &pipeSortWriteContext{
		psp: psp,
	}
	var shardNext *pipeSortProcessorShard

	for len(sh) > 1 {
		shard := sh[0]
		wctx.writeRow(shard, shard.rowRefNext)
		shard.rowRefNext++

		if shard.rowRefNext >= len(shard.rowRefs) {
			_ = heap.Pop(&sh)
			shardNext = nil

			select {
			case <-psp.stopCh:
				return nil
			default:
			}

			continue
		}

		if shardNext == nil {
			shardNext = sh[1]
			if len(sh) > 2 && sortBlockLess(sh[2], sh[2].rowRefNext, shardNext, shardNext.rowRefNext) {
				shardNext = sh[2]
			}
		}

		if sortBlockLess(shardNext, shardNext.rowRefNext, shard, shard.rowRefNext) {
			heap.Fix(&sh, 0)
			shardNext = nil

			select {
			case <-psp.stopCh:
				return nil
			default:
			}
		}
	}
	if len(sh) == 1 {
		shard := sh[0]
		for shard.rowRefNext < len(shard.rowRefs) {
			wctx.writeRow(shard, shard.rowRefNext)
			shard.rowRefNext++
		}
	}
	wctx.flush()

	return nil
}

type pipeSortWriteContext struct {
	psp *pipeSortProcessor
	rcs []resultColumn
	br  blockResult

	valuesLen int
}

func (wctx *pipeSortWriteContext) writeRow(shard *pipeSortProcessorShard, rowIdx int) {
	rr := shard.rowRefs[rowIdx]
	b := &shard.blocks[rr.blockIdx]

	byFields := shard.ps.byFields
	rcs := wctx.rcs

	areEqualColumns := len(rcs) == len(byFields)+len(b.otherColumns)
	if areEqualColumns {
		for i, c := range b.otherColumns {
			if rcs[len(byFields)+i].name != c.name {
				areEqualColumns = false
				break
			}
		}
	}
	if !areEqualColumns {
		// send the current block to bbBase and construct a block with new set of columns
		wctx.flush()

		rcs = wctx.rcs[:0]
		for _, bf := range byFields {
			rcs = append(rcs, resultColumn{
				name: bf.name,
			})
		}
		for _, c := range b.otherColumns {
			rcs = append(rcs, resultColumn{
				name: c.name,
			})
		}
		wctx.rcs = rcs
	}

	br := b.br
	byColumns := b.byColumns
	for i := range byFields {
		v := byColumns[i].c.getValueAtRow(br, rr.rowIdx)
		rcs[i].addValue(v)
		wctx.valuesLen += len(v)
	}

	for i, c := range b.otherColumns {
		v := c.getValueAtRow(br, rr.rowIdx)
		rcs[len(byFields)+i].addValue(v)
		wctx.valuesLen += len(v)
	}

	if wctx.valuesLen >= 1_000_000 {
		wctx.flush()
	}
}

func (wctx *pipeSortWriteContext) flush() {
	rcs := wctx.rcs
	br := &wctx.br

	wctx.valuesLen = 0

	if len(rcs) == 0 {
		return
	}

	// Flush rcs to ppBase
	br.setResultColumns(rcs)
	wctx.psp.ppBase.writeBlock(0, br)
	br.reset()
	for i := range rcs {
		rcs[i].resetKeepName()
	}
}

type pipeSortProcessorShardsHeap []*pipeSortProcessorShard

func (sh *pipeSortProcessorShardsHeap) Len() int {
	return len(*sh)
}

func (sh *pipeSortProcessorShardsHeap) Swap(i, j int) {
	a := *sh
	a[i], a[j] = a[j], a[i]
}

func (sh *pipeSortProcessorShardsHeap) Less(i, j int) bool {
	a := *sh
	shardA := a[i]
	shardB := a[j]
	return sortBlockLess(shardA, shardA.rowRefNext, shardB, shardB.rowRefNext)
}

func (sh *pipeSortProcessorShardsHeap) Push(x any) {
	shard := x.(*pipeSortProcessorShard)
	*sh = append(*sh, shard)
}

func (sh *pipeSortProcessorShardsHeap) Pop() any {
	a := *sh
	x := a[len(a)-1]
	a[len(a)-1] = nil
	*sh = a[:len(a)-1]
	return x
}

func sortBlockLess(shardA *pipeSortProcessorShard, rowIdxA int, shardB *pipeSortProcessorShard, rowIdxB int) bool {
	byFields := shardA.ps.byFields

	rrA := shardA.rowRefs[rowIdxA]
	rrB := shardB.rowRefs[rowIdxB]
	bA := &shardA.blocks[rrA.blockIdx]
	bB := &shardB.blocks[rrB.blockIdx]
	for idx := range bA.byColumns {
		cA := &bA.byColumns[idx]
		cB := &bB.byColumns[idx]
		isDesc := len(byFields) > 0 && byFields[idx].isDesc
		if shardA.ps.isDesc {
			isDesc = !isDesc
		}

		if cA.c.isConst && cB.c.isConst {
			// Fast path - compare const values
			ccA := cA.c.encodedValues[0]
			ccB := cB.c.encodedValues[0]
			if ccA == ccB {
				continue
			}
			return cA.c.encodedValues[0] < cB.c.encodedValues[0]
		}

		if cA.c.isTime && cB.c.isTime {
			// Fast path - sort by _time
			tA := bA.br.timestamps[rrA.rowIdx]
			tB := bB.br.timestamps[rrB.rowIdx]
			if tA == tB {
				continue
			}
			if isDesc {
				return tB < tA
			}
			return tA < tB
		}
		if cA.c.isTime {
			// treat timestamps as smaller than other values
			return true
		}
		if cB.c.isTime {
			// treat timestamps as smaller than other values
			return false
		}

		// Try sorting by int64 values at first
		uA := cA.getI64ValueAtRow(rrA.rowIdx)
		uB := cB.getI64ValueAtRow(rrB.rowIdx)
		if uA != 0 && uB != 0 {
			if uA == uB {
				continue
			}
			if isDesc {
				return uB < uA
			}
			return uA < uB
		}

		// Try sorting by float64 then
		fA := cA.getF64ValueAtRow(rrA.rowIdx)
		fB := cB.getF64ValueAtRow(rrB.rowIdx)
		if !math.IsNaN(fA) && !math.IsNaN(fB) {
			if fA == fB {
				continue
			}
			if isDesc {
				return fB < fA
			}
			return fA < fB
		}

		// Fall back to string sorting
		sA := cA.c.getValueAtRow(bA.br, rrA.rowIdx)
		sB := cB.c.getValueAtRow(bB.br, rrB.rowIdx)
		if sA == sB {
			continue
		}
		if isDesc {
			return sB < sA
		}
		return sA < sB
	}
	return false
}

func parsePipeSort(lex *lexer) (*pipeSort, error) {
	if !lex.isKeyword("sort") {
		return nil, fmt.Errorf("expecting 'sort'; got %q", lex.token)
	}
	lex.nextToken()

	var ps pipeSort
	if lex.isKeyword("by") {
		lex.nextToken()
		bfs, err := parseBySortFields(lex)
		if err != nil {
			return nil, fmt.Errorf("cannot parse 'by' clause: %w", err)
		}
		ps.byFields = bfs
	}

	if lex.isKeyword("desc") {
		lex.nextToken()
		ps.isDesc = true
	}

	return &ps, nil
}

// bySortField represents 'by (...)' part of the pipeSort.
type bySortField struct {
	// the name of the field to sort
	name string

	// whether the sorting for the given field in descending order
	isDesc bool
}

func (bf *bySortField) String() string {
	s := quoteTokenIfNeeded(bf.name)
	if bf.isDesc {
		s += " desc"
	}
	return s
}

func parseBySortFields(lex *lexer) ([]*bySortField, error) {
	if !lex.isKeyword("(") {
		return nil, fmt.Errorf("missing `(`")
	}
	var bfs []*bySortField
	for {
		lex.nextToken()
		if lex.isKeyword(")") {
			lex.nextToken()
			return bfs, nil
		}
		fieldName, err := parseFieldName(lex)
		if err != nil {
			return nil, fmt.Errorf("cannot parse field name: %w", err)
		}
		bf := &bySortField{
			name: fieldName,
		}
		if lex.isKeyword("desc") {
			lex.nextToken()
			bf.isDesc = true
		}
		bfs = append(bfs, bf)
		switch {
		case lex.isKeyword(")"):
			lex.nextToken()
			return bfs, nil
		case lex.isKeyword(","):
		default:
			return nil, fmt.Errorf("unexpected token: %q; expecting ',' or ')'", lex.token)
		}
	}
}

func marshalJSONKeyValue(dst []byte, k, v string) []byte {
	dst = strconv.AppendQuote(dst, k)
	dst = append(dst, ':')
	dst = strconv.AppendQuote(dst, v)
	return dst
}

func tryParseInt64(s string) (int64, bool) {
	if len(s) == 0 {
		return 0, false
	}

	isMinus := s[0] == '-'
	if isMinus {
		s = s[1:]
	}
	u64, ok := tryParseUint64(s)
	if !ok {
		return 0, false
	}
	if !isMinus {
		if u64 > math.MaxInt64 {
			return 0, false
		}
		return int64(u64), true
	}
	if u64 > -math.MinInt64 {
		return 0, false
	}
	return -int64(u64), true
}
