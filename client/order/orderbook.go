package order

import (
	"fmt"
	"sync"
	"sync/atomic"

	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
)

var (
	// defaultQueueCapacity represents the default capacity of
	// the order note queue.
	defaultQueueCapacity = 10
)

// Order represents an ask or bid.
type Order struct {
	OrderID  order.OrderID
	Side     uint8
	Quantity uint64
	Rate     uint64
	Time     uint64
}

// RemoteOrderBook defines the functions a client tracked order book
// must implement.
type RemoteOrderBook interface {
	// Sync instantiates a client tracked order book with the
	// current order book snapshot.
	Sync(*msgjson.OrderBook)
	// Book adds a new order to the order book.
	Book(*msgjson.BookOrderNote)
	// Unbook removes an order from the order book.
	Unbook(*msgjson.UnbookOrderNote) error
}

// CachedOrderNote represents a cached order not entry.
type cachedOrderNote struct {
	Route     string
	OrderNote interface{}
}

// OrderBook represents a client tracked order book.
type OrderBook struct {
	Seq          uint64
	MarketID     string
	NoteQueue    []*cachedOrderNote
	NoteQueueMtx sync.Mutex
	Orders       map[order.OrderID]*Order
	OrdersMtx    sync.Mutex
	Buys         *bookSide
	Sells        *bookSide
	Synced       bool
	SyncedMtx    sync.Mutex
}

// NewOrderBook creates a new order book.
func NewOrderBook() *OrderBook {
	ob := &OrderBook{
		NoteQueue: make([]*cachedOrderNote, 0, defaultQueueCapacity),
	}
	return ob
}

// setSynced sets the synced state of the order book.
func (ob *OrderBook) setSynced(value bool) {
	ob.SyncedMtx.Lock()
	ob.Synced = value
	ob.SyncedMtx.Unlock()
}

// isSynced returns the synced state of the order book.
func (ob *OrderBook) isSynced() bool {
	ob.SyncedMtx.Lock()
	defer ob.SyncedMtx.Unlock()
	return ob.Synced
}

// cacheOrderNote caches an order note.
func (ob *OrderBook) cacheOrderNote(route string, entry interface{}) error {
	note := new(cachedOrderNote)

	switch route {
	case msgjson.BookOrderRoute, msgjson.UnbookOrderRoute:
		note.Route = route
		note.OrderNote = entry

		ob.NoteQueueMtx.Lock()
		ob.NoteQueue = append(ob.NoteQueue, note)
		ob.NoteQueueMtx.Unlock()

		return nil

	default:
		return fmt.Errorf("unknown route provided %s", route)
	}
}

// processCachedNotes processes all cached notes, each processed note is
// removed from the cache.
func (ob *OrderBook) processCachedNotes() error {
	ob.NoteQueueMtx.Lock()
	defer ob.NoteQueueMtx.Unlock()

	for len(ob.NoteQueue) > 0 {
		var entry *cachedOrderNote
		entry, ob.NoteQueue = ob.NoteQueue[0], ob.NoteQueue[1:]

		switch entry.Route {
		case msgjson.BookOrderRoute:
			note, ok := entry.OrderNote.(*msgjson.BookOrderNote)
			if !ok {
				panic("failed to cast cached book order note" +
					" as a BookOrderNote")
			}
			err := ob.book(note, true)
			if err != nil {
				return err
			}

		case msgjson.UnbookOrderRoute:
			note, ok := entry.OrderNote.(*msgjson.UnbookOrderNote)
			if !ok {
				panic("failed to cast cached unbook order note" +
					" as an UnbookOrderNote")
			}
			err := ob.unbook(note, true)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown cached note route "+
				" provided: %s", entry.Route)
		}
	}

	return nil
}

// Sync updates a client tracked order book with an order
// book snapshot.
func (ob *OrderBook) Sync(snapshot *msgjson.OrderBook) error {
	if ob.isSynced() {
		return fmt.Errorf("order book is already synced")
	}

	atomic.StoreUint64(&ob.Seq, snapshot.Seq)
	ob.MarketID = snapshot.MarketID
	ob.Orders = make(map[order.OrderID]*Order)
	ob.Buys = NewBookSide(descending)
	ob.Sells = NewBookSide(ascending)

	for _, o := range snapshot.Orders {
		if len(o.OrderID) != order.OrderIDSize {
			return fmt.Errorf("order id length is not %v", order.OrderIDSize)
		}

		var oid order.OrderID
		copy(oid[:], o.OrderID)
		order := &Order{
			OrderID:  oid,
			Side:     o.Side,
			Quantity: o.Quantity,
			Rate:     o.Rate,
			Time:     o.Time,
		}

		ob.OrdersMtx.Lock()
		ob.Orders[order.OrderID] = order
		ob.OrdersMtx.Unlock()

		// Append the order to the order book.
		switch o.Side {
		case msgjson.BuyOrderNum:
			ob.Buys.Add(order)

		case msgjson.SellOrderNum:
			ob.Sells.Add(order)

		default:
			return fmt.Errorf("unknown order side provided: %d", o.Side)
		}
	}

	// Process cached order notes.
	err := ob.processCachedNotes()
	if err != nil {
		return err
	}

	ob.setSynced(true)

	return nil
}

// book is the workhorse of the exported Book function. It allows booking
// cached and uncached order notes.
func (ob *OrderBook) book(note *msgjson.BookOrderNote, cached bool) error {
	if ob.MarketID != note.MarketID {
		return fmt.Errorf("invalid note market id %s", note.MarketID)
	}

	if !cached {
		// Cache the note if the order book is not synced.
		if !ob.isSynced() {
			return ob.cacheOrderNote(msgjson.BookOrderRoute, note)
		}
	}

	// Discard a note if the order book is synced past it.
	if ob.Seq > note.Seq {
		return nil
	}

	seq := atomic.AddUint64(&ob.Seq, 1)
	if seq != note.Seq {
		return fmt.Errorf("order book out of sync, %d < %d", seq, note.Seq)
	}

	if len(note.OrderID) != order.OrderIDSize {
		return fmt.Errorf("order id length is not %d", order.OrderIDSize)
	}

	var oid order.OrderID
	copy(oid[:], note.OrderID)

	order := &Order{
		OrderID:  oid,
		Side:     note.Side,
		Quantity: note.Quantity,
		Rate:     note.Rate,
	}

	ob.OrdersMtx.Lock()
	ob.Orders[order.OrderID] = order
	ob.OrdersMtx.Unlock()

	// Add the order to its associated books side.
	switch order.Side {
	case msgjson.BuyOrderNum:
		ob.Buys.Add(order)

	case msgjson.SellOrderNum:
		ob.Sells.Add(order)

	default:
		return fmt.Errorf("unknown order side provided: %d", order.Side)
	}

	return nil
}

// Book adds a new order to the order book.
func (ob *OrderBook) Book(note *msgjson.BookOrderNote) error {
	return ob.book(note, false)
}

// unbook is the workhorse of the exported Unbook function. It allows unbooking
// cached and uncached order notes.
func (ob *OrderBook) unbook(note *msgjson.UnbookOrderNote, cached bool) error {
	if ob.MarketID != note.MarketID {
		return fmt.Errorf("invalid note market id %s", note.MarketID)
	}

	if !cached {
		// Cache the note if the order book is not synced.
		if !ob.isSynced() {
			return ob.cacheOrderNote(msgjson.UnbookOrderRoute, note)
		}
	}

	// Discard a note if the order book is synced past it.
	if ob.Seq > note.Seq {
		return nil
	}

	seq := atomic.AddUint64(&ob.Seq, 1)
	if seq != note.Seq {
		return fmt.Errorf("order book out of sync, %d < %d", seq, note.Seq)
	}

	if len(note.OrderID) != order.OrderIDSize {
		return fmt.Errorf("order id length is not %d", order.OrderIDSize)
	}

	var oid order.OrderID
	copy(oid[:], note.OrderID)

	order, ok := ob.Orders[oid]
	if !ok {
		return fmt.Errorf("no order found with id %s", oid.String())
	}

	// Remove the order from its associated book side.
	switch order.Side {
	case msgjson.BuyOrderNum:
		err := ob.Buys.Remove(order)
		if err != nil {
			return err
		}

	case msgjson.SellOrderNum:
		err := ob.Sells.Remove(order)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown order side provided: %d", order.Side)
	}

	ob.OrdersMtx.Lock()
	delete(ob.Orders, oid)
	ob.OrdersMtx.Unlock()

	return nil
}

// Unbook removes an order from the order book.
func (ob *OrderBook) Unbook(note *msgjson.UnbookOrderNote) error {
	return ob.unbook(note, false)
}

// BestNOrders returns the best n orders from the provided side.
func (ob *OrderBook) BestNOrders(n uint64, side uint8) ([]*Order, error) {
	if !ob.isSynced() {
		return nil, fmt.Errorf("order book is unsynced")
	}

	switch side {
	case msgjson.BuyOrderNum:
		return ob.Buys.BestNOrders(n)

	case msgjson.SellOrderNum:
		return ob.Sells.BestNOrders(n)

	default:
		return nil, fmt.Errorf("unknown side provided: %d", side)
	}
}

// BestFIll returns the best fill for a quantity from the provided side.
func (ob *OrderBook) BestFill(qty uint64, side uint8) ([]*fill, error) {
	if !ob.isSynced() {
		return nil, fmt.Errorf("order book is unsynced")
	}

	switch side {
	case msgjson.BuyOrderNum:
		return ob.Buys.BestFill(qty)

	case msgjson.SellOrderNum:
		return ob.Sells.BestFill(qty)

	default:
		return nil, fmt.Errorf("unknown side provided: %d", side)
	}
}
