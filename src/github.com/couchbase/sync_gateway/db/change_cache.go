package db

import (
	"container/heap"
	"errors"
	"expvar"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/couchbase/sync_gateway/auth"
	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/channels"
)

var DefaultMaxChannelLogPendingCount = 10000              // Max number of waiting sequences
var DefaultMaxChannelLogPendingWaitTime = 5 * time.Second // Max time we'll wait for a pending sequence before sending to missed queue

var DefaultMaxChannelLogSkippedWaitTime = 30 * time.Minute // Max time we'll wait for an entry in the missing before purging

// Enable keeping a channel-log for the "*" channel (channel.UserStarChannel). The only time this channel is needed is if
// someone has access to "*" (e.g. admin-party) and tracks its changes feed.
var EnableStarChannelLog = true

var changeCacheExpvars *expvar.Map

func init() {
	changeCacheExpvars = expvar.NewMap("syncGateway_changeCache")
	changeCacheExpvars.Set("maxPending", new(base.IntMax))
}

// Manages a cache of the recent change history of all channels.
type changeCache struct {
	context              *DatabaseContext
	logsDisabled         bool                     // If true, ignore incoming tap changes
	nextSequence         uint64                   // Next consecutive sequence number to add
	initialSequence      uint64                   // DB's current sequence at startup time
	receivedSeqs         map[uint64]struct{}      // Set of all sequences received
	pendingLogs          LogPriorityQueue         // Out-of-sequence entries waiting to be cached
	channelCaches        map[string]*channelCache // A cache of changes for each channel
	onChange             func(base.Set)           // Client callback that notifies of channel changes
	stopped              bool                     // Set by the Stop method
	skippedSeqs          SkippedSequenceQueue     // Skipped sequences still pending on the TAP feed
	skippedSeqLock       sync.RWMutex             // Coordinates access to skippedSeqs queue
	cacheLock            sync.RWMutex             // Coordinates access to struct fields
	options              CacheOptions             // Cache config
	lastPendingCheck     time.Time                // Scheduling for moving seqs from pending to skipped
	incomingDocChannel   chan IncomingDoc         // channel for pending incoming changes from feed
	activeProcessChannel chan bool                // tracks number of active go routines processing incoming changes
}

type IncomingDoc struct {
	id   string
	json *[]byte
}

type processEntryCallback func(base.Set) error

type LogEntry channels.LogEntry

type LogEntries []*LogEntry

// A priority-queue of LogEntries, kept ordered by increasing sequence #.
type LogPriorityQueue []*LogEntry

// An ordered queue of supporting Remove
type SkippedSequenceQueue []*SkippedSequence

type SkippedSequence struct {
	seq       uint64
	timeAdded time.Time
}

type CacheOptions struct {
	CachePendingSeqMaxWait time.Duration // Max wait for pending sequence before skipping
	CachePendingSeqMaxNum  int           // Max number of pending sequences before skipping
	CacheSkippedSeqMaxWait time.Duration // Max wait for skipped sequence before abandoning
}

//////// HOUSEKEEPING:

// Initializes a new changeCache.
// lastSequence is the last known database sequence assigned.
// onChange is an optional function that will be called to notify of channel changes.
func (c *changeCache) Init(context *DatabaseContext, lastSequence uint64, onChange func(base.Set), options CacheOptions) {
	c.context = context
	c.initialSequence = lastSequence
	c.nextSequence = lastSequence + 1
	c.onChange = onChange
	c.channelCaches = make(map[string]*channelCache, 10)
	c.receivedSeqs = make(map[uint64]struct{})

	// init cache options
	c.options = CacheOptions{
		CachePendingSeqMaxWait: DefaultMaxChannelLogPendingWaitTime,
		CachePendingSeqMaxNum:  DefaultMaxChannelLogPendingCount,
		CacheSkippedSeqMaxWait: DefaultMaxChannelLogSkippedWaitTime,
	}

	if options.CachePendingSeqMaxNum > 0 {
		c.options.CachePendingSeqMaxNum = options.CachePendingSeqMaxNum
	}

	if options.CachePendingSeqMaxWait > 0 {
		c.options.CachePendingSeqMaxWait = options.CachePendingSeqMaxWait
	}

	if options.CacheSkippedSeqMaxWait > 0 {
		c.options.CacheSkippedSeqMaxWait = options.CacheSkippedSeqMaxWait
	}

	base.LogTo("Cache", "Initializing changes cache with options %v", c.options)

	heap.Init(&c.pendingLogs)

	maxProcesses := 50000

	// incomingDocChannel stores the incoming entries from the tap feed.
	c.incomingDocChannel = make(chan IncomingDoc, 3*maxProcesses)

	// activeProcessChannel limits the number of concurrent goroutines
	c.activeProcessChannel = make(chan bool, maxProcesses)

	// Start task to work the incoming event queue and spawn a capped number of goroutines
	go func() {
		for doc := range c.incomingDocChannel {
			c.activeProcessChannel <- true
			go func(doc IncomingDoc) {
				defer func() { <-c.activeProcessChannel }()
				c.ProcessDoc(doc.id, *doc.json)
			}(doc)
		}
	}()

	// Start a background task to push pending to skipped if the feed goes quiet:
	go func() {
		for c.CheckPending() {
			time.Sleep(c.options.CachePendingSeqMaxWait / 2)
		}
	}()

	// Start a background task for channel cache pruning:
	go func() {
		for c.PruneChannelCaches() {
			//time.Sleep(c.options.CachePendingSeqMaxWait / 2)
			// TODO: exponential timing based on whether anything was pruned
			time.Sleep(5 * time.Minute)
		}
	}()

	// Start a background task for SkippedSequenceQueue housekeeping:
	/*
		go func() {
			for c.CleanSkippedSequenceQueue() {
				time.Sleep(c.options.CacheSkippedSeqMaxWait / 2)
			}
		}()
	*/
	c.lastPendingCheck = time.Now()
}

// Stops the cache. Clears its state and tells the housekeeping task to stop.
func (c *changeCache) Stop() {
	c.cacheLock.Lock()
	c.stopped = true
	c.logsDisabled = true
	c.cacheLock.Unlock()
}

// Forgets all cached changes for all channels.
func (c *changeCache) ClearLogs() {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	c.initialSequence, _ = c.context.LastSequence()
	c.channelCaches = make(map[string]*channelCache, 10)
	c.pendingLogs = nil
	heap.Init(&c.pendingLogs)
}

// If set to false, DocChanged() becomes a no-op.
func (c *changeCache) EnableChannelLogs(enable bool) {
	c.cacheLock.Lock()
	c.logsDisabled = !enable
	c.cacheLock.Unlock()
}

// Cleanup function, invoked periodically.
// Inserts pending entries that have been waiting too long.
// Removes entries older than MaxChannelLogCacheAge from the cache.
// Returns false if the changeCache has been closed.

func (c *changeCache) CheckPending() bool {

	if c.channelCaches == nil {
		return false
	}

	if time.Since(c.lastPendingCheck) > c.options.CachePendingSeqMaxWait {
		func() {
			c.cacheLock.Lock()
			defer c.cacheLock.Unlock()
			cleanStart := time.Now()
			changedChannels := c._addPendingLogs()
			if c.onChange != nil && len(changedChannels) > 0 {
				c.onChange(changedChannels)
			}
			base.WriteHistogram(changeCacheExpvars, "CleanUp-execution-time", cleanStart)
		}()
	}

	return true
}

func (c *changeCache) PruneChannelCaches() bool {

	if c.channelCaches == nil {
		return false
	}
	// Doesn't need c.lock - each channel cache will lock during prune
	// Remove old cache entries:
	for _, channelCache := range c.channelCaches {
		channelCache.pruneCache()
	}
	return true
}

// Cleanup function, invoked periodically.
// Removes skipped entries from skippedSeqs that have been waiting longer
// than MaxChannelLogMissingWaitTime from the queue.  Attempts view retrieval
// prior to removal
func (c *changeCache) CleanSkippedSequenceQueue() bool {

	foundEntries, pendingDeletes := func() ([]*LogEntry, []uint64) {
		base.IncrementExpvar(dbExpvars, "cleanskipped_waitForLock")
		c.skippedSeqLock.Lock()
		base.IncrementExpvar(dbExpvars, "cleanSkipped_hasLock")
		defer func() {
			c.skippedSeqLock.Unlock()
			base.IncrementExpvar(dbExpvars, "cleanskipped_unlock")
		}()
		var foundEntries []*LogEntry
		var pendingDeletes []uint64

		base.AddExpvar(dbExpvars, "cleanskipped_count", int64(len(c.skippedSeqs)))
		for _, skippedSeq := range c.skippedSeqs {
			if time.Since(skippedSeq.timeAdded) > c.options.CacheSkippedSeqMaxWait {
				base.IncrementExpvar(dbExpvars, "cleanskipped_expiredCount")
				options := ChangesOptions{Since: SequenceID{Seq: skippedSeq.seq}}
				queryStart := time.Now()
				entries, err := c.context.getChangesInChannelFromView("*", skippedSeq.seq, options)

				base.WriteHistogram(dbExpvars, "cleanskipped_query-total", queryStart)
				if err != nil && len(entries) > 0 {
					// Found it - store to send to the caches.
					foundEntries = append(foundEntries, entries[0])
					base.IncrementExpvar(dbExpvars, "skip_purge_view_hit")
				} else {
					base.Warn("Skipped Sequence %d didn't show up in MaxChannelLogMissingWaitTime, and isn't available from the * channel view - will be abandoned.", skippedSeq.seq)
					pendingDeletes = append(pendingDeletes, skippedSeq.seq)
					base.IncrementExpvar(dbExpvars, "skip_purge_view_miss")
				}
			} else {
				// skippedSeqs are ordered by arrival time, so can stop iterating once we find one
				// still inside the time window
				break
			}
		}
		return foundEntries, pendingDeletes
	}()

	// Add found entries

	base.AddExpvar(dbExpvars, "cleanskipped_addingFound", int64(len(foundEntries)))
	for _, entry := range foundEntries {

		base.IncrementExpvar(changeCacheExpvars, "processEntry-count-CleanSkipped")
		c.processEntry(entry)
	}

	// Purge pending deletes
	base.AddExpvar(dbExpvars, "cleanskipped_pendingDeletes", int64(len(pendingDeletes)))
	for _, sequence := range pendingDeletes {
		err := c.RemoveSkipped(sequence)
		base.IncrementExpvar(dbExpvars, "cleanskipped_removedSkipped")
		if err != nil {
			base.Warn("Error purging skipped sequence %d from skipped sequence queue, %v", sequence, err)
		} else {
			base.IncrementExpvar(dbExpvars, "abandoned_seqs")
		}
	}

	return true
}

// FOR TESTS ONLY: Blocks until the given sequence has been received.
func (c *changeCache) waitForSequence(sequence uint64) {
	var i int
	for i = 0; i < 20; i++ {
		c.cacheLock.Lock()
		nextSequence := c.nextSequence
		c.cacheLock.Unlock()
		if nextSequence >= sequence+1 {
			base.Logf("waitForSequence(%d) took %d ms", sequence, i*100)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic(fmt.Sprintf("changeCache: Sequence %d never showed up!", sequence))
}

// FOR TESTS ONLY: Blocks until the given sequence has been received.
func (c *changeCache) waitForSequenceWithMissing(sequence uint64) {
	var i int
	for i = 0; i < 20; i++ {
		c.cacheLock.Lock()
		nextSequence := c.nextSequence
		c.cacheLock.Unlock()
		if nextSequence >= sequence+1 {
			foundInMissing := false
			for _, skippedSeq := range c.skippedSeqs {
				if skippedSeq.seq == sequence {
					foundInMissing = true
					break
				}
			}
			if !foundInMissing {
				base.Logf("waitForSequence(%d) took %d ms", sequence, i*100)
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic(fmt.Sprintf("changeCache: Sequence %d never showed up!", sequence))
}

//////// ADDING CHANGES:

// Given a newly changed document (received from the tap feed), adds change entries to channels.
// The JSON must be the raw document from the bucket, with the metadata and all.
func (c *changeCache) ProcessDoc(docID string, docJSON []byte) {
	entryTime := time.Now()
	// Is this a user/role doc?
	if strings.HasPrefix(docID, auth.UserKeyPrefix) {
		c.processPrincipalDoc(docID, docJSON, true)
		return
	} else if strings.HasPrefix(docID, auth.RoleKeyPrefix) {
		c.processPrincipalDoc(docID, docJSON, false)
		return
	}

	// First unmarshal the doc (just its metadata, to save time/memory):
	doc, err := unmarshalDocumentSyncData(docJSON, false)
	if err != nil || !doc.hasValidSyncData() {
		base.Warn("changeCache: Error unmarshaling doc %q: %v", docID, err)
		return
	}

	if doc.Sequence <= c.initialSequence {
		return // Tap is sending us an old value from before I started up; ignore it
	}

	// Record a histogram of the Tap feed's lag:
	tapLag := time.Since(doc.TimeSaved) - time.Since(entryTime)
	lagMs := int(tapLag/(100*time.Millisecond)) * 100
	changeCacheExpvars.Add(fmt.Sprintf("lag-tap-%04dms", lagMs), 1)

	// If the doc update wasted any sequences due to conflicts, add empty entries for them:
	for _, seq := range doc.UnusedSequences {
		base.LogTo("Cache", "Received unused #%d for (%q / %q)", seq, docID, doc.CurrentRev)
		change := &LogEntry{
			Sequence:     seq,
			TimeReceived: time.Now(),
			TimeSaved:    time.Now(),
		}
		base.IncrementExpvar(changeCacheExpvars, "processEntry-count-UnusedSequences")
		c.processEntry(change)
	}

	if doc.TimeSaved.IsZero() {
		base.Warn("Found doc with zero timeSaved, docID=%s", docID)
	}

	// Now add the entry for the new doc revision:
	change := &LogEntry{
		Sequence:     doc.Sequence,
		DocID:        docID,
		RevID:        doc.CurrentRev,
		Flags:        doc.Flags,
		TimeReceived: time.Now(),
		TimeSaved:    doc.TimeSaved,
		Channels:     doc.Channels,
	}
	base.LogTo("Cache", "Received #%d after %3dms (%q / %q)", change.Sequence, int(tapLag/time.Millisecond), change.DocID, change.RevID)

	base.IncrementExpvar(changeCacheExpvars, "processEntry-count-DocChanged")
	changedChannels := c.processEntry(change)

	if c.onChange != nil && len(changedChannels) > 0 {
		c.onChange(changedChannels)
	}
}

func (c *changeCache) DocChanged(docID string, docJSON []byte) {

	// add doc to the pending doc queue
	c.incomingDocChannel <- IncomingDoc{id: docID, json: &docJSON}
	base.IncrementExpvar(changeCacheExpvars, "docChanged-addedToChannel")
}

func (c *changeCache) processPrincipalDoc(docID string, docJSON []byte, isUser bool) {
	// Currently the cache isn't really doing much with user docs; mostly it needs to know about
	// them because they have sequence numbers, so without them the sequence of sequences would
	// have gaps in it, causing later sequences to get stuck in the queue.
	princ, err := c.context.Authenticator().UnmarshalPrincipal(docJSON, "", 0, isUser)
	if princ == nil {
		base.Warn("changeCache: Error unmarshaling doc %q: %v", docID, err)
		return
	}
	sequence := princ.Sequence()
	if sequence <= c.initialSequence {
		return // Tap is sending us an old value from before I started up; ignore it
	}

	// Now add the (somewhat fictitious) entry:
	change := &LogEntry{
		Sequence:     sequence,
		TimeReceived: time.Now(),
		TimeSaved:    time.Now(),
	}
	if isUser {
		change.DocID = "_user/" + princ.Name()
	} else {
		change.DocID = "_role/" + princ.Name()
	}

	base.LogTo("Cache", "Received #%d (%q)", change.Sequence, change.DocID)

	base.IncrementExpvar(changeCacheExpvars, "processEntry-count-processPrincipalDoc")
	c.processEntry(change)
}

func (c *changeCache) queueEntry(change *LogEntry, callback processEntryCallback) {

}

// Handles a newly-arrived LogEntry.
func (c *changeCache) processEntry(change *LogEntry) base.Set {

	timeEntered := time.Now()
	base.IncrementExpvar(changeCacheExpvars, "processEntry-tracker-entry")
	c.cacheLock.Lock()

	base.IncrementExpvar(changeCacheExpvars, "processEntry-tracker-hasLock")
	base.WriteHistogram(changeCacheExpvars, "processEntry-lock-time", timeEntered)

	processStart := time.Now()
	//lockAcquireDelta := time.Since(timeEntered)
	//base.AddExpvar(dbExpvars, "process-entry-lock-acquire-cumulative-ns", int64(lockAcquireDelta))
	//highWatermarkLockAcquire.CasUpdate(int64(lockAcquireDelta))

	var exitType string
	defer func() {
		base.IncrementExpvar(changeCacheExpvars, "processEntry-tracker-defer-exit")
		c.cacheLock.Unlock()
		delta := time.Since(timeEntered)
		base.AddExpvar(dbExpvars, "process-entry-cumulative-ns", int64(delta))
		highWatermark.CasUpdate(int64(delta))
		base.AddExpvar(changeCacheExpvars, "processEntry-execution-cumulative", int64(time.Since(processStart)))
		base.IncrementExpvar(changeCacheExpvars, "processEntry-execution-count")
		base.WriteHistogram(changeCacheExpvars, fmt.Sprintf("processEntry-execution-time-%s", exitType), processStart)
	}()

	if c.logsDisabled {
		return nil
	}

	sequence := change.Sequence
	nextSequence := c.nextSequence
	if _, found := c.receivedSeqs[sequence]; found {
		base.AddExpvarTime(changeCacheExpvars, "processEntry-c-step1-duplicate", time.Since(processStart))
		base.IncrementExpvar(changeCacheExpvars, "processEntry-exit-duplicate")
		base.LogTo("Cache+", "  Ignoring duplicate of #%d", sequence)
		return nil
	}
	c.receivedSeqs[sequence] = struct{}{}
	// FIX: c.receivedSeqs grows monotonically. Need a way to remove old sequences.

	base.AddExpvarTime(changeCacheExpvars, "processEntry-c-step1-non-duplicate", time.Since(processStart))
	var changedChannels base.Set
	if sequence == nextSequence || nextSequence == 0 {
		// This is the expected next sequence so we can add it now:
		changedChannels = c._addToCache(change)
		// Also add any pending sequences that are now contiguous:
		changedChannels = changedChannels.Union(c._addPendingLogs())
		exitType = "next"
	} else if sequence > nextSequence {
		// There's a missing sequence (or several), so put this one on ice until it arrives:
		heap.Push(&c.pendingLogs, change)
		numPending := len(c.pendingLogs)
		base.LogTo("Cache", "  Deferring #%d (%d now waiting for #%d...#%d)",
			sequence, numPending, nextSequence, c.pendingLogs[0].Sequence-1)
		changeCacheExpvars.Get("maxPending").(*base.IntMax).SetIfMax(int64(numPending))
		if numPending > c.options.CachePendingSeqMaxNum {
			// Too many pending; add the oldest one:
			base.IncrementExpvar(dbExpvars, "pending_cache_full")
			changedChannels = c._addPendingLogs()
		} else if time.Since(c.lastPendingCheck) > c.options.CachePendingSeqMaxWait {
			changedChannels = c._addPendingLogs()
		}
		exitType = "pending"
	} else if sequence > c.initialSequence {
		// Out-of-order sequence received!
		// Remove from skipped sequence queue
		if c.RemoveSkipped(sequence) != nil {
			// Error removing from skipped sequences
			base.IncrementExpvar(dbExpvars, "late_find_fail")
			base.LogTo("Cache", "  Received unexpected out-of-order change - not in skippedSeqs (seq %d, expecting %d) doc %q / %q", sequence, nextSequence, change.DocID, change.RevID)
		} else {
			base.IncrementExpvar(dbExpvars, "late_find_success")
			base.LogTo("Cache", "  Received previously skipped out-of-order change (seq %d, expecting %d) doc %q / %q ", sequence, nextSequence, change.DocID, change.RevID)
			change.Skipped = true
		}

		base.AddExpvarTime(changeCacheExpvars, "processEntry-c-step2", time.Since(processStart))
		changedChannels = c._addToCache(change)
		base.AddExpvarTime(changeCacheExpvars, "processEntry-c-step3", time.Since(processStart))
		exitType = "skipped"
	}

	base.AddExpvarTime(changeCacheExpvars, fmt.Sprintf("processEntry-c-step4-%s", exitType), time.Since(processStart))
	base.IncrementExpvar(changeCacheExpvars, fmt.Sprintf("processEntry-exit-%s", exitType))

	return changedChannels
}

// Adds an entry to the appropriate channels' caches, returning the affected channels.  lateSequence
// flag indicates whether it was a change arriving out of sequence
func (c *changeCache) _addToCache(change *LogEntry) base.Set {

	sentToCache := time.Now()
	if change.Sequence >= c.nextSequence {
		c.nextSequence = change.Sequence + 1
	}
	if change.DocID == "" {
		return nil // this was a placeholder for an unused sequence
	}
	addedTo := make([]string, 0, 4)
	ch := change.Channels
	change.Channels = nil // not needed anymore, so free some memory

	for channelName, removal := range ch {
		if removal == nil || removal.Seq == change.Sequence {
			c._getChannelCache(channelName).addToCache(change, removal != nil)
			addedTo = append(addedTo, channelName)
		}
	}

	if EnableStarChannelLog {
		c._getChannelCache(channels.UserStarChannel).addToCache(change, false)
		addedTo = append(addedTo, channels.UserStarChannel)
	}

	// Record a histogram of the overall lag from the time the doc was saved:
	base.WriteHistogram(changeCacheExpvars, "lag-total", change.TimeSaved)

	// ...and from the time the doc was received from Tap:
	base.WriteHistogram(changeCacheExpvars, "lag-queue", change.TimeReceived)

	// ...and from the time the doc was sent for caching:
	if change.Skipped {
		base.WriteHistogram(changeCacheExpvars, "lag-caching-", sentToCache)
	} else {
		base.WriteHistogram(changeCacheExpvars, "lag-caching-skipped", sentToCache)
	}

	return base.SetFromArray(addedTo)
}

// Add the first change(s) from pendingLogs if they're the next sequence.  If not, and we've been
// waiting too long for nextSequence, move nextSequence to skipped queue.
// Returns the channels that changed.
func (c *changeCache) _addPendingLogs() base.Set {
	var changedChannels base.Set
	c.lastPendingCheck = time.Now()
	for len(c.pendingLogs) > 0 {
		change := c.pendingLogs[0]
		isNext := change.Sequence == c.nextSequence
		if change.Sequence < c.nextSequence {
			base.IncrementExpvar(changeCacheExpvars, "pending_sequence_error")
			base.IncrementExpvar(changeCacheExpvars, fmt.Sprintf("pending_sequence:%d", change.Sequence))
		}
		if isNext {
			heap.Pop(&c.pendingLogs)
			changedChannels = changedChannels.Union(c._addToCache(change))
		} else if len(c.pendingLogs) > c.options.CachePendingSeqMaxNum || time.Since(c.pendingLogs[0].TimeReceived) >= c.options.CachePendingSeqMaxWait {
			base.LogTo("Cache", "Adding #%d to skipped", c.nextSequence)
			base.IncrementExpvar(changeCacheExpvars, "outOfOrder")
			c.addToSkipped(c.nextSequence)
			c.nextSequence++
		} else {
			break
		}
	}
	return changedChannels
}

func (c *changeCache) getChannelCache(channelName string) *channelCache {
	cache := c.channelCaches[channelName]
	if cache == nil {
		c.cacheLock.Lock()
		// check if it was created while we waited for the lock
		cache = c.channelCaches[channelName]
		if cache == nil {
			cache = newChannelCache(c.context, channelName, c.initialSequence+1)
			c.channelCaches[channelName] = cache
		}
		c.cacheLock.Unlock()
	}
	return cache
}

func (c *changeCache) _getChannelCache(channelName string) *channelCache {
	cache := c.channelCaches[channelName]
	if cache == nil {
		cache = newChannelCache(c.context, channelName, c.initialSequence+1)
		c.channelCaches[channelName] = cache
	}
	return cache
}

//////// CHANGE ACCESS:

func (c *changeCache) GetChangesInChannel(channelName string, options ChangesOptions) ([]*LogEntry, error) {
	if c.stopped {
		return nil, base.HTTPErrorf(503, "Database closed")
	}
	return c.getChannelCache(channelName).GetChanges(options)
}

// Returns the sequence number the cache is up-to-date with.
func (c *changeCache) LastSequence() uint64 {
	c.cacheLock.RLock()
	defer c.cacheLock.RUnlock()
	return c.nextSequence - 1
}

func (c *changeCache) _allChannels() base.Set {
	array := make([]string, len(c.channelCaches))
	i := 0
	for name, _ := range c.channelCaches {
		array[i] = name
		i++
	}
	return base.SetFromArray(array)
}

func (c *changeCache) addToSkipped(sequence uint64) {

	base.IncrementExpvar(dbExpvars, "skipped_sequences")
	c.PushSkipped(&SkippedSequence{seq: sequence, timeAdded: time.Now()})
}

func (c *changeCache) getOldestSkippedSequence() uint64 {
	c.skippedSeqLock.RLock()
	defer c.skippedSeqLock.RUnlock()
	if len(c.skippedSeqs) > 0 {
		return c.skippedSeqs[0].seq
	} else {
		return uint64(0)
	}
}

//////// LOG PRIORITY QUEUE

func (h LogPriorityQueue) Len() int           { return len(h) }
func (h LogPriorityQueue) Less(i, j int) bool { return h[i].Sequence < h[j].Sequence }
func (h LogPriorityQueue) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *LogPriorityQueue) Push(x interface{}) {
	*h = append(*h, x.(*LogEntry))
}

func (h *LogPriorityQueue) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

//////// SKIPPED SEQUENCE QUEUE

func (c *changeCache) RemoveSkipped(x uint64) error {
	start := time.Now()
	defer base.WriteHistogram(changeCacheExpvars, "remove-skipped", start)
	c.skippedSeqLock.Lock()
	defer c.skippedSeqLock.Unlock()
	return c.skippedSeqs.Remove(x)
}

func (c *changeCache) PushSkipped(x *SkippedSequence) {
	c.skippedSeqLock.Lock()
	defer c.skippedSeqLock.Unlock()
	c.skippedSeqs.Push(x)
}

// Remove does a simple binary search to find and remove.
func (h *SkippedSequenceQueue) Remove(x uint64) error {

	i := SearchSequenceQueue(*h, x)
	if i < len(*h) && (*h)[i].seq == x {
		*h = append((*h)[:i], (*h)[i+1:]...)
		return nil
	} else {
		return errors.New("Value not found")
	}

}

// We always know that incoming missed sequence numbers will be larger than any previously
// added, so we don't need to do any sorting - just append to the slice
func (h *SkippedSequenceQueue) Push(x *SkippedSequence) error {
	// ensure valid sequence
	if len(*h) > 0 && x.seq <= (*h)[len(*h)-1].seq {
		return errors.New("Can't push sequence lower than existing maximum")
	}
	*h = append(*h, x)
	return nil
}

// Skipped Sequence version of sort.SearchInts - based on http://golang.org/src/sort/search.go?s=2959:2994#L73
func SearchSequenceQueue(a SkippedSequenceQueue, x uint64) int {
	return sort.Search(len(a), func(i int) bool { return a[i].seq >= x })
}
