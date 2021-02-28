package mediator

import (
	"context"
	"fmt"
	"log"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
)

var (
	_ IMediator        = (*Mediator)(nil)
	_ IMediatorBuilder = (*Mediator)(nil)
)

const (
	// ErrorNotEventHandler ...
	ErrorNotEventHandler = "not found handler for this event"

	// ErrorNotCommandHandler ...
	ErrorNotCommandHandler = "not found handler for this command"

	// ErrorContextTimeout ...
	ErrorContextTimeout = "context time out"

	// ErrorInvalidArgument ...
	ErrorInvalidArgument = "invalid argument"
)

// Mediator ...
type Mediator struct {
	mediatorConfig
	mut               *sync.Mutex
	eventHandlerMap   map[reflect.Type][]INotificationHandler
	commandHandlerMap map[reflect.Type]IRequestHandler
}

type mediatorConfig struct {
	logger ILogger
	pool   IRoutinePool
}

// Option ...
type Option interface {
	applyOption(config *mediatorConfig)
}

// OptionFunc ...
type OptionFunc func(config *mediatorConfig)

func (f OptionFunc) applyOption(config *mediatorConfig) { f(config) }

// SetLogger ...
func SetLogger(logger ILogger) Option {
	return OptionFunc(func(config *mediatorConfig) {
		config.logger = logger
	})
}

// SetRoutinePool ...
func SetRoutinePool(pool IRoutinePool) Option {
	return OptionFunc(func(config *mediatorConfig) {
		config.pool = pool
	})
}

// Options ...
func Options(options ...Option) Option {
	return OptionFunc(func(config *mediatorConfig) {
		for _, option := range options {
			option.applyOption(config)
		}
	})
}

// New ...
func New(options ...Option) IMediatorBuilder {
	config := &mediatorConfig{
		logger: new(DefaultLogger),
	}

	Options(options...).applyOption(config)
	if config.pool == nil {
		config.pool = NewRoutinePool(config.logger)
	}

	return &Mediator{
		mut:               &sync.Mutex{},
		eventHandlerMap:   make(map[reflect.Type][]INotificationHandler),
		commandHandlerMap: make(map[reflect.Type]IRequestHandler),
		mediatorConfig:    *config,
	}
}

// Publish ...
func (m *Mediator) Publish(ctx context.Context, event INotification) IResult {
	result := &Result{}
	if ctx == nil || event == nil {
		return result.setErr(errors.New((ErrorInvalidArgument + " ctx or event")))
	}

	handlers, ok := m.eventHandlerMap[event.Type()]
	if !ok {
		return result.setErr(
			errors.Errorf("Publish: %s -> %v", ErrorNotEventHandler, event.Type().String()),
		)
	}

	var (
		doneSilce []chan struct{}
		errNoti   = newErrorNotifacation()
	)

	for _, handler := range handlers {

		done := make(chan struct{})
		doneSilce = append(doneSilce, done)

		if poolErr := func(h INotificationHandler) error {

			return m.pool.Publish(func() {
				defer func() {
					if internalErr := recover(); internalErr != nil {
						msg := fmt.Sprintf("got panic when running %v event, cause: %v", event.Type().String(), internalErr)
						m.logger.Errorf(msg)
						errNoti.add(errors.New(msg))
						close(done)
					}
				}()

				err := h.Handle(ctx, event)
				errNoti.add(err)
				close(done)
			})

		}(handler); poolErr != nil {
			return result.setErr(poolErr)
		}

	}

	select {
	case <-waitAllDone(doneSilce):
		if errNoti.HasError() {
			return result.setErr(errNoti)
		}
		return result
	case <-ctx.Done():
		return result.setErr(ctx.Err())
	}
}

// Send ...
func (m *Mediator) Send(ctx context.Context, command IRequest) IResult {
	result := &Result{}
	if ctx == nil || command == nil {
		return result.setErr(errors.New(ErrorInvalidArgument + " ctx or command"))
	}

	handler, ok := m.commandHandlerMap[command.Type()]
	if !ok {
		return result.setErr(errors.Errorf("Send: %s -> %v", ErrorNotCommandHandler, command.Type().String()))
	}

	done := make(chan struct{})
	var (
		data interface{}
		err  error
	)

	if poolErr := m.pool.Publish(func() {
		defer func() {
			if internalErr := recover(); internalErr != nil {
				msg := fmt.Sprintf("got panic when running %v command, cause: %v", command.Type().String(), internalErr)
				m.logger.Errorf(msg)
				err = errors.New(msg)
				close(done)
			}
		}()

		data, err = handler.Handle(ctx, command)
		close(done)
	}); poolErr != nil {
		return result.setErr(poolErr)
	}

	select {
	case <-done:
		return result.
			setVal(data).
			setErr(err)
	case <-ctx.Done():
		return result.setErr(ctx.Err())
	}
}

// RegisterEventHandler ...
func (m *Mediator) RegisterEventHandler(matchingType reflect.Type, eventHandler INotificationHandler) IMediatorBuilder {
	if matchingType == nil || eventHandler == nil {
		panic(errors.New(ErrorInvalidArgument + " matchingType or eventHandler"))
	}

	m.mutex(func() {
		m.eventHandlerMap[matchingType] = append(m.eventHandlerMap[matchingType], eventHandler)
	})
	return m
}

// RegisterCommandHandler ...
func (m *Mediator) RegisterCommandHandler(matchingType reflect.Type, commandHandler IRequestHandler) IMediatorBuilder {
	if matchingType == nil || commandHandler == nil {
		panic(errors.New(ErrorInvalidArgument + " matchingType or commandHandler"))
	}

	m.mutex(func() {
		m.commandHandlerMap[matchingType] = commandHandler
	})
	return m
}

// Build ...
func (m *Mediator) Build() IMediator {
	return m
}

func (m *Mediator) mutex(fn func()) {
	m.mut.Lock()
	fn()
	m.mut.Unlock()
}

func waitAllDone(doneSlice []chan struct{}) <-chan struct{} {
	allDone := make(chan struct{})
	go func() {
		for _, done := range doneSlice {
			<-done
		}
		close(allDone)
	}()
	return allDone
}

// ErrorNotification ...
type ErrorNotification struct {
	mut    *sync.Mutex
	errors []error
}

func newErrorNotifacation() *ErrorNotification {
	return &ErrorNotification{
		mut: &sync.Mutex{},
	}
}

func (e *ErrorNotification) add(err error) {
	if err == nil {
		return
	}

	e.mut.Lock()
	e.errors = append(e.errors, err)
	e.mut.Unlock()
}

// HasError ...
func (e *ErrorNotification) HasError() bool {
	return len(e.errors) > 0
}

// Errors ...
func (e *ErrorNotification) Errors() []error {
	return e.errors
}

func (e *ErrorNotification) Error() string {
	return multierr.Combine(e.errors...).Error()
}

const (
	// DefaultPoolCap ...
	DefaultPoolCap = 1000

	// DefaultMaxPoolCap ...
	DefaultMaxPoolCap = 10000

	// DefualtPoolSubmitRetryCount ...
	DefualtPoolSubmitRetryCount = 5

	// DefaultIsBlockingPool ...
	DefaultIsBlockingPool = false
)

// DefaultRoutinePool ...
type DefaultRoutinePool struct {
	poolConfig
	logger ILogger
	pool   *ants.Pool
}

type poolConfig struct {
	initialPoolSize  int
	maxPoolSize      int
	submitRetryCount int
	isBlockingPool   bool
	logger           ILogger
}

// DefaultRoutinePoolOption ...
type DefaultRoutinePoolOption interface {
	applyOption(config *poolConfig)
}

// DefaultRoutinePoolOptionFunc ...
type DefaultRoutinePoolOptionFunc func(config *poolConfig)

func (f DefaultRoutinePoolOptionFunc) applyOption(config *poolConfig) { f(config) }

// SetInitialPoolSize ...
func SetInitialPoolSize(size int) DefaultRoutinePoolOption {
	return DefaultRoutinePoolOptionFunc(func(config *poolConfig) {
		config.initialPoolSize = size
	})
}

// SetMaxPoolSize ...
func SetMaxPoolSize(size int) DefaultRoutinePoolOption {
	return DefaultRoutinePoolOptionFunc(func(config *poolConfig) {
		config.maxPoolSize = size
	})
}

// SetSubmitRetryCount ...
func SetSubmitRetryCount(count int) DefaultRoutinePoolOption {
	return DefaultRoutinePoolOptionFunc(func(config *poolConfig) {
		config.submitRetryCount = count
	})
}

// SetIsBlockingPool ...
func SetIsBlockingPool(blocking bool) DefaultRoutinePoolOption {
	return DefaultRoutinePoolOptionFunc(func(config *poolConfig) {
		config.isBlockingPool = blocking
	})
}

// PoolOptions ...
func PoolOptions(options ...DefaultRoutinePoolOption) DefaultRoutinePoolOption {
	return DefaultRoutinePoolOptionFunc(func(config *poolConfig) {
		for _, v := range options {
			v.applyOption(config)
		}
	})
}

// NewRoutinePool ...
func NewRoutinePool(logger ILogger, options ...DefaultRoutinePoolOption) *DefaultRoutinePool {
	config := &poolConfig{
		initialPoolSize:  DefaultPoolCap,
		maxPoolSize:      DefaultMaxPoolCap,
		submitRetryCount: DefualtPoolSubmitRetryCount,
		isBlockingPool:   DefaultIsBlockingPool,
	}
	PoolOptions(options...).applyOption(config)

	pool, err := ants.NewPool(
		config.initialPoolSize,
		ants.WithPanicHandler(func(i interface{}) {
			logger.Errorf("mediator: got a panic when running handler: %v", i)
		}),
		ants.WithPreAlloc(false),
		ants.WithNonblocking(!config.isBlockingPool),
	)
	if err != nil {
		panic("can not initialize the pool: " + err.Error())
	}

	return &DefaultRoutinePool{
		pool:       pool,
		logger:     logger,
		poolConfig: *config,
	}
}

// Publish ...
func (p *DefaultRoutinePool) Publish(t ITask) error {
	var err error
	for i := 1; i <= p.submitRetryCount; i++ {
		err = p.pool.Submit(t)
		if err == nil {
			return nil
		} else if err == ants.ErrPoolOverload && p.pool.Cap() < p.maxPoolSize {

			newCap := int(math.Min(float64(p.pool.Cap()*2), float64(DefaultMaxPoolCap)))
			p.pool.Tune(newCap)
			p.logger.Printf("routine pool overload, expansion to pool cap: %d", newCap)

		} else if err == ants.ErrPoolOverload {

			p.logger.Printf("routine pool overload, and the capacity has reached the set maximum. Retry after sleep %dms, retry for the %dth time", time.Duration(i*i), i-1)
			// gradient descent
			time.Sleep(time.Millisecond * time.Duration(math.Pow(2.0, float64(i))))

		} else {
			p.logger.Errorf("routine pool error: %v", err)
			return err
		}
	}

	if err != nil {
		p.logger.Errorf("routine pool error: %v", err)
	}
	return err
}

// DefaultLogger ...
type DefaultLogger struct{}

// Printf ...
func (l *DefaultLogger) Printf(format string, messages ...interface{}) {
	log.Printf(format, messages...)
}

// Errorf ...
func (l *DefaultLogger) Errorf(format string, messages ...interface{}) {
	log.Printf(format, messages...)
}

// Result ...
type Result struct {
	err   error
	value interface{}
}

// Err ...
func (r Result) Err() error {
	return r.err
}

// Value ...
func (r Result) Value() interface{} {
	return r.value
}

// ValueT ...
func (r Result) ValueT(ptr interface{}) {
	reflect.ValueOf(ptr).Elem().Set(reflect.ValueOf(r.value))
}

// HasError ...
func (r Result) HasError() bool {
	return r.err != nil
}

// HasValue ...
func (r Result) HasValue() bool {
	return r.value != nil
}

func (r *Result) setErr(err error) *Result {
	r.err = err
	return r
}

func (r *Result) setVal(val interface{}) *Result {
	r.value = val
	return r
}
