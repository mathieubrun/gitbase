package sql

import (
	"context"
	"io"
	"math"
	"sync"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/log"
)

type key uint

const (
	// QueryKey to access query in the context.
	QueryKey key = iota
)

// Session holds the session data.
type Session interface {
	// Address of the server.
	Address() string
	// User of the session.
	User() string
	// Set session configuration.
	Set(key string, typ Type, value interface{})
	// Get session configuration.
	Get(key string) (Type, interface{})
	// GetAll returns a copy of session configuration
	GetAll() map[string]TypedValue
	// ID returns the unique ID of the connection.
	ID() uint32
}

// BaseSession is the basic session type.
type BaseSession struct {
	id     uint32
	addr   string
	user   string
	mu     sync.RWMutex
	config map[string]TypedValue
}

// User returns the current user of the session.
func (s *BaseSession) User() string { return s.user }

// Address returns the server address.
func (s *BaseSession) Address() string { return s.addr }

// Set implements the Session interface.
func (s *BaseSession) Set(key string, typ Type, value interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config[key] = TypedValue{typ, value}
}

// Get implements the Session interface.
func (s *BaseSession) Get(key string) (Type, interface{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.config[key]
	if !ok {
		return Null, nil
	}

	return v.Typ, v.Value
}

// GetAll returns a copy of session configuration
func (s *BaseSession) GetAll() map[string]TypedValue {
	m := make(map[string]TypedValue)
	s.mu.RLock()
	defer s.mu.RUnlock()

	for k, v := range s.config {
		m[k] = v
	}
	return m
}

// ID implements the Session interface.
func (s *BaseSession) ID() uint32 { return s.id }

// TypedValue is a value along with its type.
type TypedValue struct {
	Typ   Type
	Value interface{}
}

func defaultSessionConfig() map[string]TypedValue {
	return map[string]TypedValue{
		"auto_increment_increment": TypedValue{Int64, int64(1)},
		"time_zone":                TypedValue{Text, time.Local.String()},
		"system_time_zone":         TypedValue{Text, time.Local.String()},
		"max_allowed_packet":       TypedValue{Int32, math.MaxInt32},
		"sql_mode":                 TypedValue{Text, ""},
		"gtid_mode":                TypedValue{Int32, int32(0)},
		"ndbinfo_version":          TypedValue{Text, ""},
	}
}

// NewSession creates a new session with data.
func NewSession(address string, user string, id uint32) Session {
	return &BaseSession{
		id:     id,
		addr:   address,
		user:   user,
		config: defaultSessionConfig(),
	}
}

// NewBaseSession creates a new empty session.
func NewBaseSession() Session {
	return &BaseSession{config: defaultSessionConfig()}
}

// Context of the query execution.
type Context struct {
	context.Context
	Session
	pid    uint64
	tracer opentracing.Tracer
}

// ContextOption is a function to configure the context.
type ContextOption func(*Context)

// WithSession adds the given session to the context.
func WithSession(s Session) ContextOption {
	return func(ctx *Context) {
		ctx.Session = s
	}
}

// WithTracer adds the given tracer to the context.
func WithTracer(t opentracing.Tracer) ContextOption {
	return func(ctx *Context) {
		ctx.tracer = t
	}
}

// WithPid adds the given pid to the context.
func WithPid(pid uint64) ContextOption {
	return func(ctx *Context) {
		ctx.pid = pid
	}
}

// NewContext creates a new query context. Options can be passed to configure
// the context. If some aspect of the context is not configure, the default
// value will be used.
// By default, the context will have an empty base session and a noop tracer.
func NewContext(
	ctx context.Context,
	opts ...ContextOption,
) *Context {
	c := &Context{ctx, NewBaseSession(), 0, opentracing.NoopTracer{}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewEmptyContext returns a default context with default values.
func NewEmptyContext() *Context { return NewContext(context.TODO()) }

// Pid returns the process id associated with this context.
func (c *Context) Pid() uint64 { return c.pid }

// Span creates a new tracing span with the given context.
// It will return the span and a new context that should be passed to all
// childrens of this span.
func (c *Context) Span(
	opName string,
	opts ...opentracing.StartSpanOption,
) (opentracing.Span, *Context) {
	parentSpan := opentracing.SpanFromContext(c.Context)
	if parentSpan != nil {
		opts = append(opts, opentracing.ChildOf(parentSpan.Context()))
	}
	span := c.tracer.StartSpan(opName, opts...)
	ctx := opentracing.ContextWithSpan(c.Context, span)

	return span, &Context{ctx, c.Session, c.Pid(), c.tracer}
}

// WithContext returns a new context with the given underlying context.
func (c *Context) WithContext(ctx context.Context) *Context {
	return &Context{ctx, c.Session, c.Pid(), c.tracer}
}

// NewSpanIter creates a RowIter executed in the given span.
func NewSpanIter(span opentracing.Span, iter RowIter) RowIter {
	return &spanIter{
		span: span,
		iter: iter,
	}
}

type spanIter struct {
	span  opentracing.Span
	iter  RowIter
	count int
	max   time.Duration
	min   time.Duration
	total time.Duration
	done  bool
}

func (i *spanIter) updateTimings(start time.Time) {
	elapsed := time.Since(start)
	if i.max < elapsed {
		i.max = elapsed
	}

	if i.min > elapsed || i.min == 0 {
		i.min = elapsed
	}

	i.total += elapsed
}

func (i *spanIter) Next() (Row, error) {
	start := time.Now()

	row, err := i.iter.Next()
	if err == io.EOF {
		i.finish()
		return nil, err
	}

	if err != nil {
		i.finishWithError(err)
		return nil, err
	}

	i.count++
	i.updateTimings(start)
	return row, nil
}

func (i *spanIter) finish() {
	var avg time.Duration
	if i.count > 0 {
		avg = i.total / time.Duration(i.count)
	}

	i.span.FinishWithOptions(opentracing.FinishOptions{
		LogRecords: []opentracing.LogRecord{
			{
				Timestamp: time.Now(),
				Fields: []log.Field{
					log.Int("rows", i.count),
					log.String("total_time", i.total.String()),
					log.String("max_time", i.max.String()),
					log.String("min_time", i.min.String()),
					log.String("avg_time", avg.String()),
				},
			},
		},
	})
	i.done = true
}

func (i *spanIter) finishWithError(err error) {
	i.span.FinishWithOptions(opentracing.FinishOptions{
		LogRecords: []opentracing.LogRecord{
			{
				Timestamp: time.Now(),
				Fields:    []log.Field{log.String("error", err.Error())},
			},
		},
	})
	i.done = true
}

func (i *spanIter) Close() error {
	if !i.done {
		i.finish()
	}
	return i.iter.Close()
}
