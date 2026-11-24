# clock

`go.bison9.com/agent/clock` is the time source for code whose timing must be
testable. It imports only the standard library.

- `Clock` is the part of the `time` package that timing code needs: `Now`,
  `Since`, `Until`, `Sleep`, `After`, `NewTimer`, `NewTicker`, `AfterFunc`,
  and `WithTimeout`/`WithDeadline` (the `context` functions on this clock).
- `Or(c)` returns `c`, or the system clock when `c` is nil, so a component
  takes an optional `Clock` and production wiring never names one.
- `NewFake(start)` is a clock tests move by hand: `Advance`, `Set`, `Waiters`
  and `BlockUntil` (wait until n timers are pending). Its contexts end when
  the fake time passes their deadline, carry the parent's values and cause,
  show the parent's end as soon as the parent's cancel returns, and take the
  parent's deadline when it is earlier.

```go
f := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
ctx, cancel := f.WithTimeout(context.Background(), time.Minute)
defer cancel()
f.Advance(time.Minute) // ctx is now done with context.DeadlineExceeded
```
