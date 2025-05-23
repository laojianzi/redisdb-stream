package redisdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-queue/queue"
	"github.com/golang-queue/queue/core"
	"github.com/golang-queue/queue/job"

	"github.com/appleboy/com/bytesconv"
	"github.com/redis/go-redis/v9"
)

var _ core.Worker = (*Worker)(nil)

// Worker for Redis
type Worker struct {
	// redis config
	rdb       redis.Cmdable
	tasks     chan redis.XMessage
	stopFlag  int32
	stopOnce  sync.Once
	startOnce sync.Once
	stop      chan struct{}
	exit      chan struct{}
	opts      options
}

// NewWorker for struc
func NewWorker(opts ...Option) *Worker {
	var err error
	w := &Worker{
		opts:  newOptions(opts...),
		stop:  make(chan struct{}),
		exit:  make(chan struct{}),
		tasks: make(chan redis.XMessage),
	}

	if w.opts.connectionString != "" {
		options, err := redis.ParseURL(w.opts.connectionString)
		if err != nil {
			w.opts.logger.Fatal(err)
		}
		w.rdb = redis.NewClient(options)
	} else if w.opts.addr != "" {
		if w.opts.cluster {
			w.rdb = redis.NewClusterClient(&redis.ClusterOptions{
				Addrs:     strings.Split(w.opts.addr, ","),
				Username:  w.opts.username,
				Password:  w.opts.password,
				TLSConfig: w.opts.tls,
			})
		} else {
			options := &redis.Options{
				Addr:      w.opts.addr,
				Username:  w.opts.username,
				Password:  w.opts.password,
				DB:        w.opts.db,
				TLSConfig: w.opts.tls,
			}
			w.rdb = redis.NewClient(options)
		}
	}

	_, err = w.rdb.Ping(context.Background()).Result()
	if err != nil {
		w.opts.logger.Fatal(err)
	}

	return w
}

func (w *Worker) startConsumer() {
	w.startOnce.Do(func() {
		if err := w.rdb.XGroupCreateMkStream(
			context.Background(),
			w.opts.streamName,
			w.opts.group,
			"$",
		).Err(); err != nil {
			if err.Error() == "BUSYGROUP Consumer Group name already exists" {
				w.opts.logger.Info(err)
			} else {
				w.opts.logger.Error(err)
			}
		}

		go w.fetchTask()
	})
}

func (w *Worker) fetchTask() {
	for {
		select {
		case <-w.stop:
			return
		default:
		}

		ctx := context.Background()
		data, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    w.opts.group,
			Consumer: w.opts.consumer,
			Streams:  []string{w.opts.streamName, ">"},
			// count is number of entries we want to read from redis
			Count: 1,
			// we use the block command to make sure if no entry is found we wait
			// until an entry is found
			Block: w.opts.blockTime,
		}).Result()
		if err != nil {
			workerInfo := fmt.Sprintf("{streamName: %q, group: %q, consumer: %q}",
				w.opts.streamName, w.opts.group, w.opts.consumer)
			if errors.Is(err, redis.Nil) {
				w.opts.logger.Infof("no data while reading from redis stream %s", workerInfo)
			} else {
				w.opts.logger.Errorf("error while reading from redis %s %v", workerInfo, err)
			}

			continue
		}
		// we have received the data we should loop it and queue the messages
		// so that our tasks can start processing
		for _, result := range data {
			for _, message := range result.Messages {
				select {
				case w.tasks <- message:
					if err := w.rdb.XAck(ctx, w.opts.streamName, w.opts.group, message.ID).Err(); err != nil {
						w.opts.logger.Errorf("can't ack message: %s", message.ID)
					}
				case <-w.stop:
					// Todo: re-queue the task
					w.opts.logger.Info("re-queue the task: ", message.ID)
					if err := w.queue(message.Values); err != nil {
						w.opts.logger.Error("error to re-queue the task: ", message.ID)
					}
					close(w.exit)
					return
				}
			}
		}
	}
}

// Shutdown worker
func (w *Worker) Shutdown() error {
	if !atomic.CompareAndSwapInt32(&w.stopFlag, 0, 1) {
		return queue.ErrQueueShutdown
	}

	w.stopOnce.Do(func() {
		close(w.stop)

		// wait requeue
		select {
		case <-w.exit:
		case <-time.After(200 * time.Millisecond):
		}

		switch v := w.rdb.(type) {
		case *redis.Client:
			v.Close()
		case *redis.ClusterClient:
			v.Close()
		}
		close(w.tasks)
	})
	return nil
}

func (w *Worker) queue(data interface{}) error {
	ctx := context.Background()

	// Publish a message.
	err := w.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: w.opts.streamName,
		MaxLen: w.opts.maxLength,
		Values: data,
	}).Err()

	return err
}

// Queue send notification to queue
func (w *Worker) Queue(task core.TaskMessage) error {
	if atomic.LoadInt32(&w.stopFlag) == 1 {
		return queue.ErrQueueShutdown
	}

	return w.queue(map[string]interface{}{"body": bytesconv.BytesToStr(task.Bytes())})
}

// Run start the worker
func (w *Worker) Run(ctx context.Context, task core.TaskMessage) error {
	return w.opts.runFunc(ctx, task)
}

// Request a new task
func (w *Worker) Request() (core.TaskMessage, error) {
	clock := 0
	w.startConsumer()
loop:
	for {
		select {
		case task, ok := <-w.tasks:
			if !ok {
				return nil, queue.ErrQueueHasBeenClosed
			}
			var data job.Message
			_ = json.Unmarshal(bytesconv.StrToBytes(task.Values["body"].(string)), &data)
			return &data, nil
		case <-time.After(1 * time.Second):
			if clock == 5 {
				break loop
			}
			clock += 1
		}
	}

	return nil, queue.ErrNoTaskInQueue
}
