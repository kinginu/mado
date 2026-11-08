package inbox

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrBusy: a poll of this account is already running.
var ErrBusy = errors.New("a poll of this account is already running")

// maxPages bounds the immediate re-polls of one run when a collector says "more".
const maxPages = 20

// pollTimeout bounds one run of an account (all its pages).
const pollTimeout = 5 * time.Minute

// Scheduler polls the enabled accounts (DESIGN §23.3): one goroutine per account,
// the first poll after a jitter, then every interval_s plus the backoff.
type Scheduler struct {
	Store *Store
	// Keys resolves key_refs (the secrets file).
	Keys map[string]string
	// Client builds the HTTP client of an account: guarded unless allowPrivate.
	Client       func(allowPrivate bool) *http.Client
	Log          *slog.Logger
	MaxItemBytes int
	// FirstDelay is the jitter before an account's first poll (default 5–30 s).
	FirstDelay func() time.Duration
	// Now is the clock handed to collectors.
	Now func() time.Time

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	loops   map[string]*loop // account id -> its goroutine
	polling map[string]bool  // account ids with a poll in flight
}

type loop struct {
	cancel context.CancelFunc
	sig    string // what the loop was started for; a change restarts it
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Scheduler) firstDelay() time.Duration {
	if s.FirstDelay != nil {
		return s.FirstDelay()
	}
	return 5*time.Second + rand.N(25*time.Second)
}

// Start runs the loops of the enabled accounts until Stop.
func (s *Scheduler) Start() {
	s.mu.Lock()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.loops, s.polling = map[string]*loop{}, map[string]bool{}
	s.mu.Unlock()
	s.Sync()
}

// Stop ends every loop and waits for running polls.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func signature(a *Account) string {
	return strings.Join([]string{a.Kind, string(a.Config), a.KeyRef, a.OwnerSub, strconv.Itoa(a.IntervalS)}, "\x00")
}

// Sync starts loops for enabled pollable accounts, stops the others, and restarts
// the loops of accounts whose settings changed. Call it after every account change.
func (s *Scheduler) Sync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	as, err := s.Store.Accounts("")
	if err != nil {
		s.log().Error("inbox: list accounts", "err", err)
		return
	}
	want := map[string]*Account{}
	for _, a := range as {
		if a.Enabled && s.pollable(a) {
			want[a.ID] = a
		}
	}
	for id, l := range s.loops {
		if a, ok := want[id]; !ok || signature(a) != l.sig {
			l.cancel()
			delete(s.loops, id)
		}
	}
	for id, a := range want {
		if _, ok := s.loops[id]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(s.ctx)
		s.loops[id] = &loop{cancel: cancel, sig: signature(a)}
		s.wg.Add(1)
		go s.run(ctx, id)
	}
}

func (s *Scheduler) pollable(a *Account) bool {
	k, ok := KindByName(a.Kind)
	if !ok {
		return false
	}
	_, err := k.New(*a, "", Env{})
	return !errors.Is(err, ErrPassive)
}

func (s *Scheduler) run(ctx context.Context, id string) {
	defer s.wg.Done()
	wait := s.firstDelay()
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		_, err := s.poll(ctx, id)
		if ctx.Err() != nil {
			return
		}
		a, aerr := s.Store.Account(id)
		if aerr != nil {
			return
		}
		if err != nil && !errors.Is(err, ErrBusy) {
			s.log().Warn("inbox: poll failed", "account", a.Name, "kind", a.Kind, "err", err)
		}
		wait = time.Duration(a.IntervalS+a.BackoffS) * time.Second
	}
}

// PollNow runs one poll of an account out of band (the API's "Poll now"); ErrBusy
// when a poll of it is running.
func (s *Scheduler) PollNow(ctx context.Context, id string) (int, error) {
	a, err := s.Store.Account(id)
	if err != nil {
		return 0, err
	}
	if !s.pollable(a) {
		return 0, ErrPassive
	}
	return s.poll(ctx, id)
}

// poll is one run: Poll until the collector has no more (at most maxPages), each
// batch stored with its cursor. Errors set last_error and double the backoff.
func (s *Scheduler) poll(ctx context.Context, id string) (int, error) {
	s.mu.Lock()
	if s.polling == nil {
		s.polling = map[string]bool{}
	}
	if s.polling[id] {
		s.mu.Unlock()
		return 0, ErrBusy
	}
	s.polling[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.polling, id)
		s.mu.Unlock()
	}()
	a, err := s.Store.Account(id)
	if err != nil {
		return 0, err
	}
	added, err := s.collect(ctx, a)
	if ctx.Err() != nil && err != nil {
		return added, err // stopping: not the source's fault
	}
	msg, backoff := "", 0
	if err != nil {
		msg = ClipUTF8(err.Error(), 500)
		backoff = min(MaxBackoff, max(MinInterval, a.BackoffS*2))
	}
	if serr := s.Store.SaveRun(id, msg, backoff); serr != nil {
		s.log().Error("inbox: save run", "account", a.Name, "err", serr)
	}
	return added, err
}

func (s *Scheduler) collect(ctx context.Context, a *Account) (int, error) {
	k, ok := KindByName(a.Kind)
	if !ok {
		return 0, errors.New("unknown kind " + a.Kind)
	}
	key := ""
	if a.KeyRef != "" {
		key, ok = s.Keys[a.KeyRef]
		if !ok {
			return 0, errors.New("key_ref " + a.KeyRef + " is not in the secrets file")
		}
	}
	env := Env{Log: s.log().With("inbox_account", a.Name), Now: s.Now}
	if env.Now == nil {
		env.Now = time.Now
	}
	if s.Client != nil {
		env.HTTP = s.Client(a.AllowPrivate())
	} else {
		env.HTTP = &http.Client{Timeout: time.Minute}
	}
	c, err := k.New(*a, key, env)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	cursor := a.Cursor
	added := 0
	for page := 0; page < maxPages; page++ {
		items, next, more, err := c.Poll(ctx, cursor)
		if len(items) > 0 || next != nil {
			n, serr := s.Store.InsertBatch(a.ID, items, next, s.MaxItemBytes)
			if serr != nil {
				return added, serr
			}
			added += n
			if next != nil {
				cursor = next
			}
		}
		if err != nil {
			return added, err
		}
		if !more {
			break
		}
	}
	return added, nil
}
