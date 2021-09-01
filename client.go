package client

import (
	"bytes"
	"fmt"
	"log"
	"time"

	"github.com/blushft/jitsuclient/event"
	"github.com/blushft/jitsuclient/event/contexts"
	"github.com/blushft/jitsuclient/event/events"

	"gopkg.in/resty.v1"
)

type Client struct {
	options Options
	httpc   *resty.Client
	store   Store

	q  chan *event.Event
	cl chan struct{}

	copts []event.Option
}

func New(opts ...Option) (*Client, error) {
	options := defaultOptions(opts...)

	httpc := resty.New().SetHostURL(options.CollectorURL)
	store, err := NewMemStore()
	if err != nil {
		return nil, err
	}

	t := &Client{
		options: options,
		httpc:   httpc,
		store:   store,
		q:       make(chan *event.Event, options.QueueBuffer),
		cl:      make(chan struct{}),
		copts:   options.EventOptions(),
	}

	go t.run()

	return t, nil
}

func (t *Client) SetOption(opt event.Option) *Client {
	t.copts = append(t.copts, opt)
	return t
}

func (t *Client) SetUser(usr *contexts.User) *Client {
	return t.SetOption(event.WithContext(usr))
}

func (t *Client) SetGroup(grp *contexts.Group) *Client {
	return t.SetOption(event.WithContext(grp))
}

func (t *Client) Queue(evt *event.Event) {
	evt.Apply(t.eventOptions()...)

	t.q <- evt
}

func (t *Client) Action(a *contexts.Action, opts ...event.Option) {
	t.Queue(events.Action(a, opts...))
}

func (t *Client) Track(evt string, opts ...event.Option) {
	t.Queue(events.Track(evt, opts...))
}

func (t *Client) Identify(u *contexts.User, opts ...event.Option) {
	t.Queue(events.Identify(u, opts...))
}

func (t *Client) Alias(alias *contexts.Alias, usr *contexts.User, opts ...event.Option) {
	t.Queue(events.Alias(alias, usr, opts...))
}

func (t *Client) Page(page *contexts.Page, opts ...event.Option) {
	t.Queue(events.Pageview(page, opts...))
}

func (t *Client) Screen(screen *contexts.Screen, opts ...event.Option) {
	t.Queue(events.Screen(screen, opts...))
}

func (t *Client) Session(sess *contexts.Session, opts ...event.Option) {
	t.Queue(events.Session(sess, opts...))
}

func (t *Client) Group(g *contexts.Group, u *contexts.User, opts ...event.Option) {
	t.Queue(events.Group(g, u, opts...))
}

func (t *Client) Timing(te *contexts.Timing, opts ...event.Option) {
	t.Queue(events.Timing(te, opts...))
}

func (t *Client) Flush() {
	c, err := t.emit()
	if err != nil {
		log.Printf("error emitting events: %v\n", err)
	}

	if c > 0 && t.options.Debug {
		log.Printf("emitted %d events", c)
	}
}

func (t *Client) Close() {
	close(t.cl)

	t.Flush()
}

func (t *Client) eventOptions(opts ...event.Option) []event.Option {
	var eopts []event.Option

	eopts = append(eopts, t.copts...)
	eopts = append(eopts, opts...)

	return eopts
}

func (t *Client) run() {
	tick := time.NewTicker(t.options.FlushInterval)
	defer tick.Stop()

	for {
		select {
		case e := <-t.q:
			if t.options.Strict {
				if !e.Validate() {
					log.Printf("event failed validation")
					continue
				}
			}

			if err := t.store.Set(e); err != nil {
				log.Printf("error storing event: %s\n", err.Error())
				continue
			}

			if t.store.Count() >= t.options.FlushCount {
				t.Flush()
			}
		case <-tick.C:
			t.Flush()
		case <-t.cl:
			return
		}
	}
}

func (t *Client) emit() (int, error) {
	if t.store.Count() == 0 {
		return 0, nil
	}

	if t.options.Bulk {
		return t.emitBulk()
	}

	evts, err := t.store.GetAll()
	if err != nil {
		return 0, err
	}

	i := 0
	for _, e := range evts {
		var rm bool
		var err error

		if serr := t.send(e.Event); serr == nil {
			i++
			rm = true
		} else {
			rm, err = t.sendFailed(e, serr)
			if err != nil {
				log.Println(err.Error())
			}
		}

		if rm {
			if err := t.store.Remove(e); err != nil {
				log.Printf("error removing event: %v", err)
			}
		}
	}

	return i, nil
}

func (t *Client) emitBulk() (int, error) {
	start := time.Now()

	evts, err := t.store.GetAll()
	if err != nil {
		return 0, err
	}

	bulk := make([][]byte, len(evts))
	for c, e := range evts {
		bulk[c] = e.Event
	}

	body := bytes.Join(bulk, []byte("\n"))

	headers := t.options.clientHeaders()

	req := t.httpc.R().
		SetFileReader("file", "file", bytes.NewReader(body)).
		SetHeaders(headers).
		SetQueryParams(t.options.apiQueryParams())

	resp, err := req.Post(t.options.apiPath())

	failed := err != nil || resp.StatusCode() > 299
	if t.options.Debug {
		if err != nil {
			log.Printf("error sending bulk: %v", err)
		}

		if resp.StatusCode() > 299 {
			log.Printf("http response: code=%d body=%s", resp.StatusCode(), resp.Body())
		}
	}

	t.logDebug("bulk send complete: dur=%dms", time.Since(start).Milliseconds())

	i := 0

	for _, e := range evts {
		var rm bool
		var err error

		if failed {
			rm, err = t.sendFailed(e, err)
			if err != nil {
				log.Println(err.Error())
			}
		} else {
			i++
			rm = true
		}

		if rm {
			if err := t.store.Remove(e); err != nil {
				log.Printf("error removing event: %v", err)
			}
		}
	}

	return i, nil
}

func (t *Client) send(e []byte) error {
	start := time.Now()
	headers := t.options.clientHeaders()

	resp, err := t.httpc.R().
		SetBody(e).
		SetHeaders(headers).
		SetQueryParams(t.options.apiQueryParams()).
		Post(t.options.apiPath())

	if resp.StatusCode() > 299 {
		return fmt.Errorf("send failed with status code %d", resp.StatusCode())
	}

	t.logDebug("event send complete: dur=%dms", time.Since(start).Milliseconds())

	return err
}

func (t *Client) sendFailed(e *StoreEvent, sendErr error) (bool, error) {
	rm := false

	e.Attempted = true
	e.Attempts++
	e.LastAttempt = time.Now()

	if t.options.Debug {
		log.Printf("event failed to send: %v", sendErr)
	}

	if t.options.MaxRetries > 0 && e.Attempts > t.options.MaxRetries {
		rm = true
	} else {
		if err := t.store.Update(e); err != nil {
			return rm, fmt.Errorf("error updating event: %w", err)
		}
	}

	return rm, nil
}

func (c *Client) logDebug(msg string, args ...interface{}) {
	if c.options.Debug {
		log.Printf(msg, args...)
	}

}
