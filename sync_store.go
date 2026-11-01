package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// All reservations share one hash so overlapping paths are checked atomically,
// including across web replicas. The hash tag also supports Redis Cluster.
type redisSyncStore struct {
	client *redis.Client
	key    string
}

type syncRecord struct {
	ID string `json:"id"`
	SyncResult
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Stop    bool   `json:"stop"`
	Expires int64  `json:"expires"`
}

func openSyncStore(ctx context.Context, config Config) (*redisSyncStore, error) {
	options, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("redis URL: %w", err)
	}
	options.DialTimeout = 5 * time.Second
	options.ReadTimeout = 5 * time.Second
	options.WriteTimeout = 5 * time.Second
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	return &redisSyncStore{client: client, key: config.RedisPrefix + "{sync}:records"}, nil
}

const activeLua = `
local now = tonumber(redis.call('TIME')[1])
local function active(r) return r.state == 'queued' or r.state == 'running' or r.state == 'removing' end
local function overlap(a,b)
 return a == b or a == '/' or b == '/' or string.sub(a,1,#b+1) == b..'/' or string.sub(b,1,#a+1) == a..'/'
end
`

var reserveScript = redis.NewScript(activeLua + `
local records = redis.call('HGETALL', KEYS[1])
for i=1,#records,2 do
 local r = cjson.decode(records[i+1])
 if r.expires <= now then redis.call('HDEL',KEYS[1],records[i])
 elseif active(r) and overlap(r.path,ARGV[2]) then return 0 end
end
local r = {id=ARGV[1],path=ARGV[2],state=ARGV[3],stop=false,expires=now+600,progress=0,speed=0,downloaded=0,time_left=''}
redis.call('HSET',KEYS[1],ARGV[1],cjson.encode(r))
return 1
`)

func (s *redisSyncStore) reserve(ctx context.Context, path, state string) (string, bool, error) {
	id := uuid.NewString()
	n, err := reserveScript.Run(ctx, s.client, []string{s.key}, id, path, state).Int()
	return id, n == 1, err
}

// A worker must claim once. Kubernetes retries or duplicate invocations cannot
// run a second rsync against the same reservation.
var updateScript = redis.NewScript(activeLua + `
local raw = redis.call('HGET',KEYS[1],ARGV[1])
if not raw then return redis.error_reply('sync reservation missing') end
local r = cjson.decode(raw)
if r.expires <= now or not active(r) then return redis.error_reply('sync lease expired') end
local op = ARGV[2]
if op == 'claim' then
 if r.state ~= 'queued' then return redis.error_reply('sync already claimed') end
 r.state = 'running'
elseif op == 'launch_failed' then
 r.error=ARGV[3]
 if r.state == 'queued' then r.state='failed' else r.stop=true end
elseif op == 'finish' then
 r.state=ARGV[3]; r.error=ARGV[4]
elseif op == 'progress' then
 local p=cjson.decode(ARGV[3])
 r.progress=p.progress; r.speed=p.speed; r.downloaded=p.downloaded; r.time_left=p.time_left
end
r.expires=now+120
if not active(r) then r.expires=now+86400 end
redis.call('HSET',KEYS[1],ARGV[1],cjson.encode(r))
return cjson.encode(r)
`)

func (s *redisSyncStore) update(ctx context.Context, id, op string, args ...interface{}) (syncRecord, error) {
	argv := []interface{}{id, op}
	argv = append(argv, args...)
	raw, err := updateScript.Run(ctx, s.client, []string{s.key}, argv...).Text()
	var r syncRecord
	if err == nil {
		err = json.Unmarshal([]byte(raw), &r)
	}
	return r, err
}
func (s *redisSyncStore) finish(id, state string, err error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	message := ""
	if err != nil {
		message = err.Error()
	}
	_, e := s.update(ctx, id, "finish", state, message)
	return e
}

var listScript = redis.NewScript(activeLua + `
local result = {}
local records=redis.call('HGETALL',KEYS[1])
for i=1,#records,2 do
 local r=cjson.decode(records[i+1])
 if r.expires <= now then redis.call('HDEL',KEYS[1],records[i])
 elseif active(r) and r.state ~= 'removing' then table.insert(result,records[i+1]) end
end
return result
`)

func (s *redisSyncStore) list(ctx context.Context) ([]SyncResult, error) {
	rows, err := listScript.Run(ctx, s.client, []string{s.key}).StringSlice()
	result := make([]SyncResult, 0, len(rows))
	for _, raw := range rows {
		var r syncRecord
		if e := json.Unmarshal([]byte(raw), &r); e != nil {
			return nil, e
		}
		result = append(result, r.SyncResult)
	}
	return result, err
}

var stopScript = redis.NewScript(activeLua + `
local records=redis.call('HGETALL',KEYS[1])
for i=1,#records,2 do
 local r=cjson.decode(records[i+1])
 if r.path == ARGV[1] and r.expires > now and (r.state == 'queued' or r.state == 'running') then
 r.stop=true; redis.call('HSET',KEYS[1],records[i],cjson.encode(r))
 end
end
return 1
`)

func (s *redisSyncStore) stop(ctx context.Context, path string) error {
	return stopScript.Run(ctx, s.client, []string{s.key}, path).Err()
}
