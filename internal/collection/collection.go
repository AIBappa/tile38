package collection

import (
	"runtime"

	"github.com/tidwall/btree"
	"github.com/tidwall/geojson"
	"github.com/tidwall/geojson/geo"
	"github.com/tidwall/geojson/geometry"
	"github.com/tidwall/rtree"
	"github.com/tidwall/tile38/internal/deadline"
	"github.com/tidwall/tile38/internal/field"
)

// yieldStep forces the iterator to yield goroutine every 256 steps.
const yieldStep = 256

// Cursor allows for quickly paging through Scan, Within, Intersects, and Nearby
type Cursor interface {
	Offset() uint64
	Step(count uint64)
}

type itemT struct {
	id      string
	obj     geojson.Object
	expires int64 // unix nano expiration
	fields  field.List
}

func byID(a, b *itemT) bool {
	return a.id < b.id
}

func byValue(a, b *itemT) bool {
	value1 := a.obj.String()
	value2 := b.obj.String()
	if value1 < value2 {
		return true
	}
	if value1 > value2 {
		return false
	}
	// the values match so we'll compare IDs, which are always unique.
	return byID(a, b)
}

func byExpires(a, b *itemT) bool {
	if a.expires < b.expires {
		return true
	}
	if a.expires > b.expires {
		return false
	}
	// the values match so we'll compare IDs, which are always unique.
	return byID(a, b)
}

func (item *itemT) Rect() geometry.Rect {
	if item.obj != nil {
		return item.obj.Rect()
	}
	return geometry.Rect{}
}

// Collection represents a collection of geojson objects.
type Collection struct {
	items    *btree.BTreeG[*itemT]           // items sorted by id
	spatial  *rtree.RTreeGN[float32, *itemT] // items geospatially indexed
	values   *btree.BTreeG[*itemT]           // items sorted by value+id
	expires  *btree.BTreeG[*itemT]           // items sorted by ex+id
	weight   int
	points   int
	objects  int // geometry count
	nobjects int // non-geometry count
}

var optsNoLock = btree.Options{NoLocks: true}

// New creates an empty collection
func New() *Collection {
	col := &Collection{
		items:   btree.NewBTreeGOptions(byID, optsNoLock),
		values:  btree.NewBTreeGOptions(byValue, optsNoLock),
		expires: btree.NewBTreeGOptions(byExpires, optsNoLock),
		spatial: &rtree.RTreeGN[float32, *itemT]{},
	}
	return col
}

// Count returns the number of objects in collection.
func (c *Collection) Count() int {
	return c.objects + c.nobjects
}

// StringCount returns the number of string values.
func (c *Collection) StringCount() int {
	return c.nobjects
}

// PointCount returns the number of points (lat/lon coordinates) in collection.
func (c *Collection) PointCount() int {
	return c.points
}

// TotalWeight calculates the in-memory cost of the collection in bytes.
func (c *Collection) TotalWeight() int {
	return c.weight
}

// Bounds returns the bounds of all the items in the collection.
func (c *Collection) Bounds() (minX, minY, maxX, maxY float64) {
	_, _, left := c.spatial.LeftMost()
	_, _, bottom := c.spatial.BottomMost()
	_, _, right := c.spatial.RightMost()
	_, _, top := c.spatial.TopMost()
	if left == nil {
		return
	}
	return left.Rect().Min.X, bottom.Rect().Min.Y,
		right.Rect().Max.X, top.Rect().Max.Y
}

func objIsSpatial(obj geojson.Object) bool {
	_, ok := obj.(geojson.Spatial)
	return ok
}

func (c *Collection) objWeight(item *itemT) int {
	var weight int
	weight += len(item.id)
	if objIsSpatial(item.obj) {
		weight += item.obj.NumPoints() * 16
	} else {
		weight += len(item.obj.String())
	}
	weight += item.fields.Weight()
	return weight
}

func (c *Collection) indexDelete(item *itemT) {
	if !item.obj.Empty() {
		c.spatial.Delete(rtreeItem(item))
	}
}

func (c *Collection) indexInsert(item *itemT) {
	if !item.obj.Empty() {
		c.spatial.Insert(rtreeItem(item))
	}
}

const dRNDTOWARDS = (1.0 - 1.0/8388608.0) /* Round towards zero */
const dRNDAWAY = (1.0 + 1.0/8388608.0)    /* Round away from zero */

func rtreeValueDown(d float64) float32 {
	f := float32(d)
	if float64(f) > d {
		if d < 0 {
			f = float32(d * dRNDAWAY)
		} else {
			f = float32(d * dRNDTOWARDS)
		}
	}
	return f
}
func rtreeValueUp(d float64) float32 {
	f := float32(d)
	if float64(f) < d {
		if d < 0 {
			f = float32(d * dRNDTOWARDS)
		} else {
			f = float32(d * dRNDAWAY)
		}
	}
	return f
}

func rtreeItem(item *itemT) (min, max [2]float32, data *itemT) {
	min, max = rtreeRect(item.Rect())
	return min, max, item
}

func rtreeRect(rect geometry.Rect) (min, max [2]float32) {
	return [2]float32{
			rtreeValueDown(rect.Min.X),
			rtreeValueDown(rect.Min.Y),
		}, [2]float32{
			rtreeValueUp(rect.Max.X),
			rtreeValueUp(rect.Max.Y),
		}
}

// Set adds or replaces an object in the collection and returns the fields
// array.
func (c *Collection) Set(id string, obj geojson.Object, fields field.List, ex int64) (
	oldObject geojson.Object, oldFields, newFields field.List,
) {
	newItem := &itemT{
		id:      id,
		obj:     obj,
		expires: ex,
		fields:  fields,
	}

	// add the new item to main btree and remove the old one if needed
	oldItem, ok := c.items.Set(newItem)
	if ok {
		// the old item was removed, now let's remove it from the rtree/btree.
		if objIsSpatial(oldItem.obj) {
			c.indexDelete(oldItem)
			c.objects--
		} else {
			c.values.Delete(oldItem)
			c.nobjects--
		}
		// delete old item from the expires queue
		if oldItem.expires != 0 {
			c.expires.Delete(oldItem)
		}

		// decrement the point count
		c.points -= oldItem.obj.NumPoints()

		// decrement the weights
		c.weight -= c.objWeight(oldItem)
	}

	// insert the new item into the rtree or strings tree.
	if objIsSpatial(newItem.obj) {
		c.indexInsert(newItem)
		c.objects++
	} else {
		c.values.Set(newItem)
		c.nobjects++
	}
	// insert item into expires queue.
	if newItem.expires != 0 {
		c.expires.Set(newItem)
	}

	// increment the point count
	c.points += newItem.obj.NumPoints()

	// add the new weights
	c.weight += c.objWeight(newItem)

	if oldItem != nil {
		return oldItem.obj, oldItem.fields, newItem.fields
	}
	return nil, field.List{}, newItem.fields
}

// Delete removes an object and returns it.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) Delete(id string) (
	obj geojson.Object, fields field.List, ok bool,
) {
	oldItem, ok := c.items.Delete(&itemT{id: id})
	if !ok {
		return nil, field.List{}, false
	}
	if objIsSpatial(oldItem.obj) {
		if !oldItem.obj.Empty() {
			c.indexDelete(oldItem)
		}
		c.objects--
	} else {
		c.values.Delete(oldItem)
		c.nobjects--
	}
	// delete old item from expires queue
	if oldItem.expires != 0 {
		c.expires.Delete(oldItem)
	}
	c.weight -= c.objWeight(oldItem)
	c.points -= oldItem.obj.NumPoints()

	return oldItem.obj, oldItem.fields, true
}

// Get returns an object.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) Get(id string) (
	obj geojson.Object,
	fields field.List,
	ex int64,
	ok bool,
) {
	item, ok := c.items.Get(&itemT{id: id})
	if !ok {
		return nil, field.List{}, 0, false
	}
	return item.obj, item.fields, item.expires, true
}

// Scan iterates though the collection ids.
func (c *Collection) Scan(
	desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields field.List) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(item *itemT) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		keepon = iterator(item.id, item.obj, item.fields)
		return keepon
	}
	if desc {
		c.items.Reverse(iter)
	} else {
		c.items.Scan(iter)
	}
	return keepon
}

// ScanRange iterates though the collection starting with specified id.
func (c *Collection) ScanRange(
	start, end string,
	desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields field.List) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(item *itemT) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		if !desc {
			if item.id >= end {
				return false
			}
		} else {
			if item.id <= end {
				return false
			}
		}
		keepon = iterator(item.id, item.obj, item.fields)
		return keepon
	}

	if desc {
		c.items.Descend(&itemT{id: start}, iter)
	} else {
		c.items.Ascend(&itemT{id: start}, iter)
	}
	return keepon
}

// SearchValues iterates though the collection values.
func (c *Collection) SearchValues(
	desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields field.List) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(item *itemT) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		keepon = iterator(item.id, item.obj, item.fields)
		return keepon
	}
	if desc {
		c.values.Reverse(iter)
	} else {
		c.values.Scan(iter)
	}
	return keepon
}

// SearchValuesRange iterates though the collection values.
func (c *Collection) SearchValuesRange(start, end string, desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields field.List) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(item *itemT) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		keepon = iterator(item.id, item.obj, item.fields)
		return keepon
	}
	pstart := &itemT{obj: String(start)}
	pend := &itemT{obj: String(end)}
	if desc {
		// descend range
		c.values.Descend(pstart, func(item *itemT) bool {
			return bGT(c.values, item, pend) && iter(item)
		})
	} else {
		c.values.Ascend(pstart, func(item *itemT) bool {
			return bLT(c.values, item, pend) && iter(item)
		})
	}
	return keepon
}

func bLT(tr *btree.BTreeG[*itemT], a, b *itemT) bool { return tr.Less(a, b) }
func bGT(tr *btree.BTreeG[*itemT], a, b *itemT) bool { return tr.Less(b, a) }

// ScanGreaterOrEqual iterates though the collection starting with specified id.
func (c *Collection) ScanGreaterOrEqual(id string, desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields field.List, ex int64) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(item *itemT) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		keepon = iterator(item.id, item.obj, item.fields, item.expires)
		return keepon
	}
	if desc {
		c.items.Descend(&itemT{id: id}, iter)
	} else {
		c.items.Ascend(&itemT{id: id}, iter)
	}
	return keepon
}

func (c *Collection) geoSearch(
	rect geometry.Rect,
	iter func(id string, obj geojson.Object, fields field.List) bool,
) bool {
	alive := true
	min, max := rtreeRect(rect)
	c.spatial.Search(
		min, max,
		func(_, _ [2]float32, item *itemT) bool {
			alive = iter(item.id, item.obj, item.fields)
			return alive
		},
	)
	return alive
}

func (c *Collection) geoSparse(
	obj geojson.Object, sparse uint8,
	iter func(id string, obj geojson.Object, fields field.List) (match, ok bool),
) bool {
	matches := make(map[string]bool)
	alive := true
	c.geoSparseInner(obj.Rect(), sparse,
		func(id string, o geojson.Object, fields field.List) (
			match, ok bool,
		) {
			ok = true
			if !matches[id] {
				match, ok = iter(id, o, fields)
				if match {
					matches[id] = true
				}
			}
			return match, ok
		},
	)
	return alive
}
func (c *Collection) geoSparseInner(
	rect geometry.Rect, sparse uint8,
	iter func(id string, obj geojson.Object, fields field.List) (match, ok bool),
) bool {
	if sparse > 0 {
		w := rect.Max.X - rect.Min.X
		h := rect.Max.Y - rect.Min.Y
		quads := [4]geometry.Rect{
			{
				Min: geometry.Point{X: rect.Min.X, Y: rect.Min.Y + h/2},
				Max: geometry.Point{X: rect.Min.X + w/2, Y: rect.Max.Y},
			},
			{
				Min: geometry.Point{X: rect.Min.X + w/2, Y: rect.Min.Y + h/2},
				Max: geometry.Point{X: rect.Max.X, Y: rect.Max.Y},
			},
			{
				Min: geometry.Point{X: rect.Min.X, Y: rect.Min.Y},
				Max: geometry.Point{X: rect.Min.X + w/2, Y: rect.Min.Y + h/2},
			},
			{
				Min: geometry.Point{X: rect.Min.X + w/2, Y: rect.Min.Y},
				Max: geometry.Point{X: rect.Max.X, Y: rect.Min.Y + h/2},
			},
		}
		for _, quad := range quads {
			if !c.geoSparseInner(quad, sparse-1, iter) {
				return false
			}
		}
		return true
	}
	alive := true
	c.geoSearch(rect,
		func(id string, obj geojson.Object, fields field.List) bool {
			match, ok := iter(id, obj, fields)
			if !ok {
				alive = false
				return false
			}
			return !match
		},
	)
	return alive
}

// Within returns all object that are fully contained within an object or
// bounding box. Set obj to nil in order to use the bounding box.
func (c *Collection) Within(
	obj geojson.Object,
	sparse uint8,
	cursor Cursor,
	deadline *deadline.Deadline,
	iter func(id string, obj geojson.Object, fields field.List) bool,
) bool {
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	if sparse > 0 {
		return c.geoSparse(obj, sparse,
			func(id string, o geojson.Object, fields field.List) (
				match, ok bool,
			) {
				count++
				if count <= offset {
					return false, true
				}
				nextStep(count, cursor, deadline)
				if match = o.Within(obj); match {
					ok = iter(id, o, fields)
				}
				return match, ok
			},
		)
	}
	return c.geoSearch(obj.Rect(),
		func(id string, o geojson.Object, fields field.List) bool {
			count++
			if count <= offset {
				return true
			}
			nextStep(count, cursor, deadline)
			if o.Within(obj) {
				return iter(id, o, fields)
			}
			return true
		},
	)
}

// Intersects returns all object that are intersect an object or bounding box.
// Set obj to nil in order to use the bounding box.
func (c *Collection) Intersects(
	obj geojson.Object,
	sparse uint8,
	cursor Cursor,
	deadline *deadline.Deadline,
	iter func(id string, obj geojson.Object, fields field.List) bool,
) bool {
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	if sparse > 0 {
		return c.geoSparse(obj, sparse,
			func(id string, o geojson.Object, fields field.List) (
				match, ok bool,
			) {
				count++
				if count <= offset {
					return false, true
				}
				nextStep(count, cursor, deadline)
				if match = o.Intersects(obj); match {
					ok = iter(id, o, fields)
				}
				return match, ok
			},
		)
	}
	return c.geoSearch(obj.Rect(),
		func(id string, o geojson.Object, fields field.List) bool {
			count++
			if count <= offset {
				return true
			}
			nextStep(count, cursor, deadline)
			if o.Intersects(obj) {
				return iter(id, o, fields)
			}
			return true
		},
	)
}

// Nearby returns the nearest neighbors
func (c *Collection) Nearby(
	target geojson.Object,
	cursor Cursor,
	deadline *deadline.Deadline,
	iter func(id string, obj geojson.Object, fields field.List, dist float64) bool,
) bool {
	// First look to see if there's at least one candidate in the circle's
	// outer rectangle. This is a fast-fail operation.
	if circle, ok := target.(*geojson.Circle); ok {
		meters := circle.Meters()
		if meters > 0 {
			center := circle.Center()
			minLat, minLon, maxLat, maxLon :=
				geo.RectFromCenter(center.Y, center.X, meters)
			var exists bool
			min, max := rtreeRect(geometry.Rect{
				Min: geometry.Point{
					X: minLon,
					Y: minLat,
				},
				Max: geometry.Point{
					X: maxLon,
					Y: maxLat,
				},
			})
			c.spatial.Search(
				min, max,
				func(_, _ [2]float32, item *itemT) bool {
					exists = true
					return false
				},
			)
			if !exists {
				// no candidates
				return true
			}
		}
	}
	// do the kNN operation
	alive := true
	center := target.Center()
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	distFn := geodeticDistAlgo[*itemT]([2]float64{center.X, center.Y})
	c.spatial.Nearby(
		func(min, max [2]float32, data *itemT, item bool) float32 {
			return float32(distFn(
				[2]float64{float64(min[0]), float64(min[1])},
				[2]float64{float64(max[0]), float64(max[1])},
				data, item,
			))
		},
		func(_, _ [2]float32, item *itemT, dist float32) bool {
			count++
			if count <= offset {
				return true
			}
			nextStep(count, cursor, deadline)
			alive = iter(item.id, item.obj, item.fields, float64(dist))
			return alive
		},
	)
	return alive
}

func nextStep(step uint64, cursor Cursor, deadline *deadline.Deadline) {
	if step&(yieldStep-1) == (yieldStep - 1) {
		runtime.Gosched()
		deadline.Check()
	}
	if cursor != nil {
		cursor.Step(1)
	}
}

// ScanExpires returns a list of all objects that have expired.
func (c *Collection) ScanExpires(iter func(id string, expires int64) bool) {
	c.expires.Scan(func(item *itemT) bool {
		return iter(item.id, item.expires)
	})
}
