package redismock

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/go-redis/redis/v7"
)

type mock struct {
	ctx context.Context

	parent *mock
	client *redis.Client

	factory  *redis.Client
	expected []expectation

	strictOrder bool

	expectRegexp bool
	expectCustom CustomMatch
}

func NewClientMock() (*redis.Client, ClientMock) {
	opt := &redis.Options{
		// set -2, avoid executing commands on the redis server
		MaxRetries: -2,
	}
	m := &mock{
		ctx:     context.Background(),
		client:  redis.NewClient(opt),
		factory: redis.NewClient(opt),
	}
	m.client.AddHook(redisClientHook(m.process))
	m.strictOrder = true

	return m.client, m
}

//----------------------------------

type redisClientHook func(cmd redis.Cmder) error

func (fh redisClientHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	return ctx, fh(cmd)
}

func (fh redisClientHook) AfterProcess(_ context.Context, _ redis.Cmder) error {
	return nil
}

func (fh redisClientHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	for _, cmd := range cmds {
		if err := fh(cmd); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

func (fh redisClientHook) AfterProcessPipeline(_ context.Context, _ []redis.Cmder) error {
	return nil
}

//----------------------------------

func (m *mock) process(cmd redis.Cmder) (err error) {
	var miss int
	var expect expectation = nil

	for _, e := range m.expected {
		e.lock()

		// not available, has been matched
		if !e.usable() {
			e.unlock()
			miss++
			continue
		}

		err = m.match(e, cmd)

		// matched
		if err == nil {
			expect = e
			break
		}

		// strict order of command execution
		if m.strictOrder {
			e.unlock()
			cmd.SetErr(err)
			return err
		}
		e.unlock()
	}

	if expect == nil {
		msg := "call to cmd '%+v' was not expected"
		if miss == len(m.expected) {
			msg = "all expectations were already fulfilled, " + msg
		}
		err = fmt.Errorf(msg, cmd.Args())
		cmd.SetErr(err)
		return err
	}

	defer expect.unlock()

	expect.trigger()

	// write error
	if err = expect.error(); err != nil {
		cmd.SetErr(err)
		return err
	}

	// write redis.Nil
	if expect.isRedisNil() {
		err = redis.Nil
		cmd.SetErr(err)
		return err
	}

	// if do not set error or redis.Nil, must set val
	if !expect.isSetVal() {
		err = fmt.Errorf("cmd(%s), return value is required", expect.name())
		cmd.SetErr(err)
		return err
	}

	cmd.SetErr(nil)
	expect.inflow(cmd)

	return nil
}

func (m *mock) match(expect expectation, cmd redis.Cmder) error {
	expectArgs := expect.args()
	cmdArgs := cmd.Args()

	if len(expectArgs) != len(cmdArgs) {
		return fmt.Errorf("parameters do not match, expectation '%+v', but call to cmd '%+v'", expectArgs, cmdArgs)
	}

	if expect.name() != cmd.Name() {
		return fmt.Errorf("command not match, expectation '%s', but call to cmd '%s'", expect.name(), cmd.Name())
	}

	// custom func match
	if fn := expect.custom(); fn != nil {
		return fn(expectArgs, cmdArgs)
	}

	for i := 0; i < len(expectArgs); i++ {
		expr, ok := expectArgs[i].(string)

		// regular
		if expect.regexp() && ok {
			cmdValue := fmt.Sprint(cmdArgs[i])
			re, err := regexp.Compile(expr)
			if err != nil {
				return err
			}
			if !re.MatchString(cmdValue) {
				return fmt.Errorf("%d column does not match, expectation regular: '%s', but gave: '%s'", i, expr, cmdValue)
			}
		} else if !reflect.DeepEqual(expectArgs[i], cmdArgs[i]) {
			return fmt.Errorf("%d column does not `DeepEqual`, expectation: '%+v', but gave: '%+v'",
				i, expectArgs[i], cmdArgs[i])
		}
	}

	return nil
}

func (m *mock) pushExpect(e expectation) {
	if m.expectRegexp {
		e.setRegexpMatch()
	}
	if m.expectCustom != nil {
		e.setCustomMatch(m.expectCustom)
	}
	if m.parent != nil {
		m.parent.pushExpect(e)
		return
	}
	m.expected = append(m.expected, e)
}

func (m *mock) ClearExpect() {
	if m.parent != nil {
		m.parent.ClearExpect()
		return
	}
	m.expected = nil
}

func (m *mock) Regexp() *mock {
	if m.parent != nil {
		return m.parent.Regexp()
	}
	clone := *m
	clone.parent = m
	clone.expectRegexp = true

	return &clone
}

func (m *mock) CustomMatch(fn CustomMatch) *mock {
	if m.parent != nil {
		return m.parent.CustomMatch(fn)
	}
	clone := *m
	clone.parent = m
	clone.expectCustom = fn

	return &clone
}

func (m *mock) ExpectationsWereMet() error {
	if m.parent != nil {
		return m.ExpectationsWereMet()
	}
	for _, e := range m.expected {
		e.lock()
		usable := e.usable()
		e.unlock()

		if usable {
			return fmt.Errorf("there is a remaining expectation which was not matched: %+v", e.args())
		}
	}
	return nil
}

func (m *mock) MatchExpectationsInOrder(b bool) {
	if m.parent != nil {
		m.MatchExpectationsInOrder(b)
		return
	}
	m.strictOrder = b
}

func (m *mock) ExpectTxPipeline() {
	e := &ExpectedStatus{}
	e.cmd = redis.NewStatusCmd("multi")
	e.SetVal("OK")
	m.pushExpect(e)
}

func (m *mock) ExpectTxPipelineExec() *ExpectedSlice {
	e := &ExpectedSlice{}
	e.cmd = redis.NewSliceCmd("exec")
	e.SetVal(nil)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectWatch(keys ...string) *ExpectedError {
	e := &ExpectedError{}
	args := make([]interface{}, 1+len(keys))
	args[0] = "watch"
	for i, key := range keys {
		args[1+i] = key
	}
	e.cmd = redis.NewStatusCmd(args...)
	e.setVal = true
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectCommand() *ExpectedCommandsInfo {
	e := &ExpectedCommandsInfo{}
	e.cmd = m.factory.Command()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClientGetName() *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.ClientGetName()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectEcho(message interface{}) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.Echo(message)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPing() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Ping()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectQuit() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Quit()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectDel(keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Del(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectUnlink(keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Unlink(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectDump(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.Dump(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectExists(keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Exists(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectExpire(key string, expiration time.Duration) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.Expire(key, expiration)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectExpireAt(key string, tm time.Time) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.ExpireAt(key, tm)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectKeys(pattern string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.Keys(pattern)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectMigrate(host, port, key string, db int, timeout time.Duration) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Migrate(host, port, key, db, timeout)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectMove(key string, db int) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.Move(key, db)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectObjectRefCount(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ObjectRefCount(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectObjectEncoding(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.ObjectEncoding(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectObjectIdleTime(key string) *ExpectedDuration {
	e := &ExpectedDuration{}
	e.cmd = m.factory.ObjectIdleTime(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPersist(key string) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.Persist(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPExpire(key string, expiration time.Duration) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.PExpire(key, expiration)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPExpireAt(key string, tm time.Time) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.PExpireAt(key, tm)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPTTL(key string) *ExpectedDuration {
	e := &ExpectedDuration{}
	e.cmd = m.factory.PTTL(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRandomKey() *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.RandomKey()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRename(key, newkey string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Rename(key, newkey)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRenameNX(key, newkey string) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.RenameNX(key, newkey)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRestore(key string, ttl time.Duration, value string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Restore(key, ttl, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRestoreReplace(key string, ttl time.Duration, value string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.RestoreReplace(key, ttl, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSort(key string, sort *redis.Sort) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.Sort(key, sort)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSortStore(key, store string, sort *redis.Sort) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SortStore(key, store, sort)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSortInterfaces(key string, sort *redis.Sort) *ExpectedSlice {
	e := &ExpectedSlice{}
	e.cmd = m.factory.SortInterfaces(key, sort)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectTouch(keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Touch(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectTTL(key string) *ExpectedDuration {
	e := &ExpectedDuration{}
	e.cmd = m.factory.TTL(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectType(key string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Type(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectAppend(key, value string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Append(key, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectDecr(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Decr(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectDecrBy(key string, decrement int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.DecrBy(key, decrement)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGet(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.Get(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGetRange(key string, start, end int64) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.GetRange(key, start, end)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGetSet(key string, value interface{}) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.GetSet(key, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectIncr(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Incr(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectIncrBy(key string, value int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.IncrBy(key, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectIncrByFloat(key string, value float64) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.IncrByFloat(key, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectMGet(keys ...string) *ExpectedSlice {
	e := &ExpectedSlice{}
	e.cmd = m.factory.MGet(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectMSet(values ...interface{}) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.MSet(values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectMSetNX(values ...interface{}) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.MSetNX(values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSet(key string, value interface{}, expiration time.Duration) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Set(key, value, expiration)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSetNX(key string, value interface{}, expiration time.Duration) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.SetNX(key, value, expiration)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSetXX(key string, value interface{}, expiration time.Duration) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.SetXX(key, value, expiration)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSetRange(key string, offset int64, value string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SetRange(key, offset, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectStrLen(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.StrLen(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGetBit(key string, offset int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.GetBit(key, offset)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSetBit(key string, offset int64, value int) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SetBit(key, offset, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBitCount(key string, bitCount *redis.BitCount) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.BitCount(key, bitCount)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBitOpAnd(destKey string, keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.BitOpAnd(destKey, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBitOpOr(destKey string, keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.BitOpOr(destKey, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBitOpXor(destKey string, keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.BitOpXor(destKey, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBitOpNot(destKey string, key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.BitOpNot(destKey, key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBitPos(key string, bit int64, pos ...int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.BitPos(key, bit, pos...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBitField(key string, args ...interface{}) *ExpectedIntSlice {
	e := &ExpectedIntSlice{}
	e.cmd = m.factory.BitField(key, args...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectScan(cursor uint64, match string, count int64) *ExpectedScan {
	e := &ExpectedScan{}
	e.cmd = m.factory.Scan(cursor, match, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSScan(key string, cursor uint64, match string, count int64) *ExpectedScan {
	e := &ExpectedScan{}
	e.cmd = m.factory.SScan(key, cursor, match, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHScan(key string, cursor uint64, match string, count int64) *ExpectedScan {
	e := &ExpectedScan{}
	e.cmd = m.factory.HScan(key, cursor, match, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZScan(key string, cursor uint64, match string, count int64) *ExpectedScan {
	e := &ExpectedScan{}
	e.cmd = m.factory.ZScan(key, cursor, match, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHDel(key string, fields ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.HDel(key, fields...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHExists(key, field string) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.HExists(key, field)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHGet(key, field string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.HGet(key, field)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHGetAll(key string) *ExpectedStringStringMap {
	e := &ExpectedStringStringMap{}
	e.cmd = m.factory.HGetAll(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHIncrBy(key, field string, incr int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.HIncrBy(key, field, incr)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHIncrByFloat(key, field string, incr float64) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.HIncrByFloat(key, field, incr)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHKeys(key string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.HKeys(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHLen(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.HLen(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHMGet(key string, fields ...string) *ExpectedSlice {
	e := &ExpectedSlice{}
	e.cmd = m.factory.HMGet(key, fields...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHSet(key string, values ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.HSet(key, values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHMSet(key string, values ...interface{}) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.HMSet(key, values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHSetNX(key, field string, value interface{}) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.HSetNX(key, field, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectHVals(key string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.HVals(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBLPop(timeout time.Duration, keys ...string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.BLPop(timeout, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBRPop(timeout time.Duration, keys ...string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.BRPop(timeout, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBRPopLPush(source, destination string, timeout time.Duration) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.BRPopLPush(source, destination, timeout)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLIndex(key string, index int64) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.LIndex(key, index)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLInsert(key, op string, pivot, value interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LInsert(key, op, pivot, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLInsertBefore(key string, pivot, value interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LInsertBefore(key, pivot, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLInsertAfter(key string, pivot, value interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LInsertAfter(key, pivot, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLLen(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LLen(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLPop(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.LPop(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLPush(key string, values ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LPush(key, values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLPushX(key string, values ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LPushX(key, values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLRange(key string, start, stop int64) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.LRange(key, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLRem(key string, count int64, value interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LRem(key, count, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLSet(key string, index int64, value interface{}) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.LSet(key, index, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLTrim(key string, start, stop int64) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.LTrim(key, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRPop(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.RPop(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRPopLPush(source, destination string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.RPopLPush(source, destination)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRPush(key string, values ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.RPush(key, values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectRPushX(key string, values ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.RPushX(key, values...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSAdd(key string, members ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SAdd(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSCard(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SCard(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSDiff(keys ...string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.SDiff(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSDiffStore(destination string, keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SDiffStore(destination, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSInter(keys ...string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.SInter(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSInterStore(destination string, keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SInterStore(destination, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSIsMember(key string, member interface{}) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.SIsMember(key, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSMembers(key string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.SMembers(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSMembersMap(key string) *ExpectedStringStructMap {
	e := &ExpectedStringStructMap{}
	e.cmd = m.factory.SMembersMap(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSMove(source, destination string, member interface{}) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.SMove(source, destination, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSPop(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.SPop(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSPopN(key string, count int64) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.SPopN(key, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSRandMember(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.SRandMember(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSRandMemberN(key string, count int64) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.SRandMemberN(key, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSRem(key string, members ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SRem(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSUnion(keys ...string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.SUnion(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSUnionStore(destination string, keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.SUnionStore(destination, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXAdd(a *redis.XAddArgs) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.XAdd(a)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXDel(stream string, ids ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.XDel(stream, ids...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXLen(stream string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.XLen(stream)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXRange(stream, start, stop string) *ExpectedXMessageSlice {
	e := &ExpectedXMessageSlice{}
	e.cmd = m.factory.XRange(stream, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXRangeN(stream, start, stop string, count int64) *ExpectedXMessageSlice {
	e := &ExpectedXMessageSlice{}
	e.cmd = m.factory.XRangeN(stream, start, stop, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXRevRange(stream string, start, stop string) *ExpectedXMessageSlice {
	e := &ExpectedXMessageSlice{}
	e.cmd = m.factory.XRevRange(stream, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXRevRangeN(stream string, start, stop string, count int64) *ExpectedXMessageSlice {
	e := &ExpectedXMessageSlice{}
	e.cmd = m.factory.XRevRangeN(stream, start, stop, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXRead(a *redis.XReadArgs) *ExpectedXStreamSlice {
	e := &ExpectedXStreamSlice{}
	e.cmd = m.factory.XRead(a)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXReadStreams(streams ...string) *ExpectedXStreamSlice {
	e := &ExpectedXStreamSlice{}
	e.cmd = m.factory.XReadStreams(streams...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXGroupCreate(stream, group, start string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.XGroupCreate(stream, group, start)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXGroupCreateMkStream(stream, group, start string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.XGroupCreateMkStream(stream, group, start)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXGroupSetID(stream, group, start string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.XGroupSetID(stream, group, start)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXGroupDestroy(stream, group string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.XGroupDestroy(stream, group)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXGroupDelConsumer(stream, group, consumer string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.XGroupDelConsumer(stream, group, consumer)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXReadGroup(a *redis.XReadGroupArgs) *ExpectedXStreamSlice {
	e := &ExpectedXStreamSlice{}
	e.cmd = m.factory.XReadGroup(a)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXAck(stream, group string, ids ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.XAck(stream, group, ids...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXPending(stream, group string) *ExpectedXPending {
	e := &ExpectedXPending{}
	e.cmd = m.factory.XPending(stream, group)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXPendingExt(a *redis.XPendingExtArgs) *ExpectedXPendingExt {
	e := &ExpectedXPendingExt{}
	e.cmd = m.factory.XPendingExt(a)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXClaim(a *redis.XClaimArgs) *ExpectedXMessageSlice {
	e := &ExpectedXMessageSlice{}
	e.cmd = m.factory.XClaim(a)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXClaimJustID(a *redis.XClaimArgs) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.XClaimJustID(a)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXTrim(key string, maxLen int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.XTrim(key, maxLen)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXTrimApprox(key string, maxLen int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.XTrimApprox(key, maxLen)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectXInfoGroups(key string) *ExpectedXInfoGroups {
	e := &ExpectedXInfoGroups{}
	e.cmd = m.factory.XInfoGroups(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBZPopMax(timeout time.Duration, keys ...string) *ExpectedZWithKey {
	e := &ExpectedZWithKey{}
	e.cmd = m.factory.BZPopMax(timeout, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBZPopMin(timeout time.Duration, keys ...string) *ExpectedZWithKey {
	e := &ExpectedZWithKey{}
	e.cmd = m.factory.BZPopMin(timeout, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZAdd(key string, members ...*redis.Z) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZAdd(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZAddNX(key string, members ...*redis.Z) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZAddNX(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZAddXX(key string, members ...*redis.Z) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZAddXX(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZAddCh(key string, members ...*redis.Z) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZAddCh(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZAddNXCh(key string, members ...*redis.Z) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZAddNXCh(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZAddXXCh(key string, members ...*redis.Z) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZAddXXCh(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZIncr(key string, member *redis.Z) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.ZIncr(key, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZIncrNX(key string, member *redis.Z) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.ZIncrNX(key, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZIncrXX(key string, member *redis.Z) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.ZIncrXX(key, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZCard(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZCard(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZCount(key, min, max string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZCount(key, min, max)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZLexCount(key, min, max string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZLexCount(key, min, max)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZIncrBy(key string, increment float64, member string) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.ZIncrBy(key, increment, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZInterStore(destination string, store *redis.ZStore) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZInterStore(destination, store)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZPopMax(key string, count ...int64) *ExpectedZSlice {
	e := &ExpectedZSlice{}
	e.cmd = m.factory.ZPopMax(key, count...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZPopMin(key string, count ...int64) *ExpectedZSlice {
	e := &ExpectedZSlice{}
	e.cmd = m.factory.ZPopMin(key, count...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRange(key string, start, stop int64) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ZRange(key, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRangeWithScores(key string, start, stop int64) *ExpectedZSlice {
	e := &ExpectedZSlice{}
	e.cmd = m.factory.ZRangeWithScores(key, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRangeByScore(key string, opt *redis.ZRangeBy) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ZRangeByScore(key, opt)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRangeByLex(key string, opt *redis.ZRangeBy) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ZRangeByLex(key, opt)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRangeByScoreWithScores(key string, opt *redis.ZRangeBy) *ExpectedZSlice {
	e := &ExpectedZSlice{}
	e.cmd = m.factory.ZRangeByScoreWithScores(key, opt)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRank(key, member string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZRank(key, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRem(key string, members ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZRem(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRemRangeByRank(key string, start, stop int64) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZRemRangeByRank(key, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRemRangeByScore(key, min, max string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZRemRangeByScore(key, min, max)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRemRangeByLex(key, min, max string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZRemRangeByLex(key, min, max)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRevRange(key string, start, stop int64) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ZRevRange(key, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRevRangeWithScores(key string, start, stop int64) *ExpectedZSlice {
	e := &ExpectedZSlice{}
	e.cmd = m.factory.ZRevRangeWithScores(key, start, stop)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRevRangeByScore(key string, opt *redis.ZRangeBy) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ZRevRangeByScore(key, opt)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRevRangeByLex(key string, opt *redis.ZRangeBy) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ZRevRangeByLex(key, opt)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRevRangeByScoreWithScores(key string, opt *redis.ZRangeBy) *ExpectedZSlice {
	e := &ExpectedZSlice{}
	e.cmd = m.factory.ZRevRangeByScoreWithScores(key, opt)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZRevRank(key, member string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZRevRank(key, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZScore(key, member string) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.ZScore(key, member)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectZUnionStore(dest string, store *redis.ZStore) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ZUnionStore(dest, store)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPFAdd(key string, els ...interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.PFAdd(key, els...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPFCount(keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.PFCount(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPFMerge(dest string, keys ...string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.PFMerge(dest, keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBgRewriteAOF() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.BgRewriteAOF()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectBgSave() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.BgSave()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClientKill(ipPort string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClientKill(ipPort)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClientKillByFilter(keys ...string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ClientKillByFilter(keys...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClientList() *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.ClientList()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClientPause(dur time.Duration) *ExpectedBool {
	e := &ExpectedBool{}
	e.cmd = m.factory.ClientPause(dur)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClientID() *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ClientID()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectConfigGet(parameter string) *ExpectedSlice {
	e := &ExpectedSlice{}
	e.cmd = m.factory.ConfigGet(parameter)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectConfigResetStat() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ConfigResetStat()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectConfigSet(parameter, value string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ConfigSet(parameter, value)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectConfigRewrite() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ConfigRewrite()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectDBSize() *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.DBSize()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectFlushAll() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.FlushAll()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectFlushAllAsync() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.FlushAllAsync()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectFlushDB() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.FlushDB()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectFlushDBAsync() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.FlushDBAsync()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectInfo(section ...string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.Info(section...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectLastSave() *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.LastSave()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSave() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Save()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectShutdown() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.Shutdown()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectShutdownSave() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ShutdownSave()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectShutdownNoSave() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ShutdownNoSave()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectSlaveOf(host, port string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.SlaveOf(host, port)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectTime() *ExpectedTime {
	e := &ExpectedTime{}
	e.cmd = m.factory.Time()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectDebugObject(key string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.DebugObject(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectReadOnly() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ReadOnly()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectReadWrite() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ReadWrite()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectMemoryUsage(key string, samples ...int) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.MemoryUsage(key, samples...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectEval(script string, keys []string, args ...interface{}) *ExpectedCmd {
	e := &ExpectedCmd{}
	e.cmd = m.factory.Eval(script, keys, args...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectEvalSha(sha1 string, keys []string, args ...interface{}) *ExpectedCmd {
	e := &ExpectedCmd{}
	e.cmd = m.factory.EvalSha(sha1, keys, args...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectScriptExists(hashes ...string) *ExpectedBoolSlice {
	e := &ExpectedBoolSlice{}
	e.cmd = m.factory.ScriptExists(hashes...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectScriptFlush() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ScriptFlush()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectScriptKill() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ScriptKill()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectScriptLoad(script string) *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.ScriptLoad(script)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPublish(channel string, message interface{}) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.Publish(channel, message)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPubSubChannels(pattern string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.PubSubChannels(pattern)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPubSubNumSub(channels ...string) *ExpectedStringIntMap {
	e := &ExpectedStringIntMap{}
	e.cmd = m.factory.PubSubNumSub(channels...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectPubSubNumPat() *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.PubSubNumPat()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterSlots() *ExpectedClusterSlots {
	e := &ExpectedClusterSlots{}
	e.cmd = m.factory.ClusterSlots()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterNodes() *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.ClusterNodes()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterMeet(host, port string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterMeet(host, port)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterForget(nodeID string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterForget(nodeID)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterReplicate(nodeID string) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterReplicate(nodeID)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterResetSoft() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterResetSoft()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterResetHard() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterResetHard()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterInfo() *ExpectedString {
	e := &ExpectedString{}
	e.cmd = m.factory.ClusterInfo()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterKeySlot(key string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ClusterKeySlot(key)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterGetKeysInSlot(slot int, count int) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ClusterGetKeysInSlot(slot, count)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterCountFailureReports(nodeID string) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ClusterCountFailureReports(nodeID)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterCountKeysInSlot(slot int) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.ClusterCountKeysInSlot(slot)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterDelSlots(slots ...int) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterDelSlots(slots...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterDelSlotsRange(min, max int) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterDelSlotsRange(min, max)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterSaveConfig() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterSaveConfig()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterSlaves(nodeID string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.ClusterSlaves(nodeID)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterFailover() *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterFailover()
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterAddSlots(slots ...int) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterAddSlots(slots...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectClusterAddSlotsRange(min, max int) *ExpectedStatus {
	e := &ExpectedStatus{}
	e.cmd = m.factory.ClusterAddSlotsRange(min, max)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoAdd(key string, geoLocation ...*redis.GeoLocation) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.GeoAdd(key, geoLocation...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoPos(key string, members ...string) *ExpectedGeoPos {
	e := &ExpectedGeoPos{}
	e.cmd = m.factory.GeoPos(key, members...)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoRadius(
	key string, longitude, latitude float64, query *redis.GeoRadiusQuery,
) *ExpectedGeoLocation {
	e := &ExpectedGeoLocation{}
	e.cmd = m.factory.GeoRadius(key, longitude, latitude, query)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoRadiusStore(
	key string, longitude, latitude float64, query *redis.GeoRadiusQuery,
) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.GeoRadiusStore(key, longitude, latitude, query)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoRadiusByMember(key, member string, query *redis.GeoRadiusQuery) *ExpectedGeoLocation {
	e := &ExpectedGeoLocation{}
	e.cmd = m.factory.GeoRadiusByMember(key, member, query)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoRadiusByMemberStore(key, member string, query *redis.GeoRadiusQuery) *ExpectedInt {
	e := &ExpectedInt{}
	e.cmd = m.factory.GeoRadiusByMemberStore(key, member, query)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoDist(key string, member1, member2, unit string) *ExpectedFloat {
	e := &ExpectedFloat{}
	e.cmd = m.factory.GeoDist(key, member1, member2, unit)
	m.pushExpect(e)
	return e
}

func (m *mock) ExpectGeoHash(key string, members ...string) *ExpectedStringSlice {
	e := &ExpectedStringSlice{}
	e.cmd = m.factory.GeoHash(key, members...)
	m.pushExpect(e)
	return e
}
