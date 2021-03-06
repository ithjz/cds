package tube

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/tal-tech/go-zero/core/logx"
)

//配置文件
type SubscriberConf struct {
	Brokers      []string `json:"Brokers"`
	Topic        string
	Group        string
	SliceSize    int
	WorkerNum    int `json:",default=32"`
	TimerPeriod  int
	ThrottleSize int `json:",default=209715200"`
}

type partitionMessages struct {
	lastCommitMsg kafka.Message
	commitMsg     kafka.Message
	outputted     map[int64]kafka.Message // app layer consumed
}

type messages struct {
	sync.RWMutex
	data map[int]partitionMessages // partition -> messages
}

type kfkStreamConsumer struct {
	ctx         context.Context
	handleFunc  HandleFunc
	kafkaReader *kafka.Reader
	producer    *kafka.Writer // error backup
	workerNum   int
	pm          messages
}

// MustNewKfkStreamConsumer constructor of KfkStreamConsumer
func MustNewKfkStreamConsumer(Topic, Group string, workerNum int, Brokers []string) KfkStreamConsumer {
	done := checkConnect(Topic, Brokers)
	if done {
		log.Fatal("wrong Topic or Brokers")
	}
	config := kafka.ReaderConfig{
		Brokers:          Brokers,
		GroupID:          Group,
		Topic:            Topic,
		MinBytes:         10e3, // 10KB
		MaxBytes:         10e6, // 10MB
		MaxWait:          10 * time.Second,
		SessionTimeout:   10 * time.Second,
		RebalanceTimeout: 5 * 60 * time.Second,
	}
	r := kafka.NewReader(config)
	cw := kafka.WriterConfig{
		Brokers:  Brokers,
		Topic:    Topic + "-failed",
		Balancer: &kafka.LeastBytes{},
		Async:    true,
	}
	w := kafka.NewWriter(cw)
	ptm := make(map[int]partitionMessages)
	return &kfkStreamConsumer{
		pm:          messages{data: ptm},
		kafkaReader: r,
		producer:    w,
		workerNum:   workerNum,
	}
}

// check kafka connection
func checkConnect(Topic string, Brokers []string) bool {
	for _, broker := range Brokers {
		if c, err := kafka.DialLeader(
			context.Background(),
			"tcp",
			broker,
			Topic,
			0); err != nil {
			logx.Error(err)
			return true
		} else if _, err := c.ReadFirstOffset(); err != nil {
			logx.Error(err)
			return true
		}
	}
	return false
}

func (ks *kfkStreamConsumer) Subscribe(ctx context.Context, handle HandleFunc) chan interface{} {
	workerCh := make(chan interface{})
	ks.handleFunc = handle
	ks.ctx = ctx

	go ks.runWorkers(ctx, workerCh)
	return workerCh
}

func (ks *kfkStreamConsumer) Commit() error {
	ks.findCommitPoint()
	return ks.commit()
}

func (ks *kfkStreamConsumer) Close() error {
	return ks.kafkaReader.Close()
}

// 保持一定数量的协程同时工作，每个协程处理完一个消息后等待workerch通道接收，然后退出。
func (ks *kfkStreamConsumer) runWorkers(ctx context.Context, workerChan chan interface{}) {
	goroutineLimit := make(chan int, ks.workerNum)
	for {
		timeout, cancelFunc := context.WithTimeout(ctx, time.Second*3)
		go ks.fetchWorker(timeout, goroutineLimit, workerChan)
		select {
		case goroutineLimit <- 1:
		case <-ctx.Done():
			close(workerChan)
			cancelFunc()
			return
		}
	}
}

// commit offset
func (ks *kfkStreamConsumer) commit() error {
	ks.pm.Lock()
	defer ks.pm.Unlock()
	for partition, msges := range ks.pm.data {
		if msges.commitMsg.Offset == msges.lastCommitMsg.Offset {
			//logx.Info(msges.commitMsg)
			logx.Infof("partition %d has committed", partition)
			continue
		}
		retry := 0
	commitCheckpoint:
		for {
			if err := ks.kafkaReader.CommitMessages(ks.ctx, msges.commitMsg); err != nil {
				logx.Error(err)
				time.Sleep(time.Second)
				retry++
				if retry > 3 {
					return errors.New("can not commit")
				}
			} else {
				logx.Infof("commit [topic]: %s, [partition]: %d, [offset]: %d\n", msges.commitMsg.Topic, msges.commitMsg.Partition, msges.commitMsg.Offset)
				msges.lastCommitMsg = msges.commitMsg
				ks.pm.data[partition] = msges
				retry = 0
				break commitCheckpoint
			}
		}
	}
	return nil
}

// find the continuously outputted messages' offset
func (ks *kfkStreamConsumer) findCommitPoint() {
	ks.pm.Lock()
	defer ks.pm.Unlock()
	for p, pmp := range ks.pm.data {
		// 某分区
		km := ks.pm.data[p].lastCommitMsg
		i := pmp.commitMsg.Offset + 1
		//查找被应用层连续消费的消息的最大offset
		for {
			if m, ok := pmp.outputted[i]; ok {
				//logx.Info(m)
				//如果在这个partition的所有被应用层消费的msg中找到了这个partition的恢复点的下一个offset
				//则更新恢复点的offset
				//因为找到它了，为了节省内存，加速查找，所以删除它
				delete(pmp.outputted, i)
				km = m
				//去找下一个
				i++
			} else {
				//没找到或是不再能找到了
				pmp.commitMsg = km
				ks.pm.data[p] = pmp
				break
			}
		}
	}
}

// fetch message and store outputted message
func (ks *kfkStreamConsumer) fetchWorker(ctx context.Context, goroutineLimit chan int, workerChan chan interface{}) {
	defer func() {
		if recover() != nil {
			logx.Error("channel closed")
		}
		select {
		case <-goroutineLimit:
		case <-ctx.Done():
			time.Sleep(time.Second)
			<-goroutineLimit
		}
	}()

	kafkaMsg, failed := ks.fetchWithRetry(ctx)
	if failed {
		return
	}
	result, err := ks.handleFunc(kafkaMsg.Value)
	if err != nil {
		logx.Error(err)
		err := ks.producer.WriteMessages(ctx, kafka.Message{
			Key:   kafkaMsg.Key,
			Value: kafkaMsg.Value,
		})
		if err != nil {
			logx.Error(err)
		}
	} else {
		workerChan <- result
	}
	ks.storeConsumedMsg(kafkaMsg)
}

// store consumed message by app layer for finding continuously offset to commit
func (ks *kfkStreamConsumer) storeConsumedMsg(kafkaMsg kafka.Message) {
	//kafka-go 提交需要 kafka.Message 结构.不保存value，用于减小内存
	kafkaMsg.Value = []byte{}
	ks.pm.Lock()
	defer ks.pm.Unlock()
	if pmm, ok := ks.pm.data[kafkaMsg.Partition]; !ok {
		// ks.pm.data没有这个分区的数据，那么这应该是这个分区下载的第一条message，同时存入准备提交的结构中
		pm := partitionMessages{
			lastCommitMsg: kafka.Message{},
			commitMsg:     kafkaMsg,
			outputted:     make(map[int64]kafka.Message),
		}
		pm.outputted[kafkaMsg.Offset] = kafkaMsg
		ks.pm.data[kafkaMsg.Partition] = pm
	} else {
		// 保存 Msg 用于查找提交
		pmm.outputted[kafkaMsg.Offset] = kafkaMsg
	}
}

// fetch kafka message with retry but don't commit offset
func (ks *kfkStreamConsumer) fetchWithRetry(ctx context.Context) (kafka.Message, bool) {
	var kafkaMsg kafka.Message
	var err error
	fetchRetryCnt := 0
fetch:
	for {
		kafkaMsg, err = ks.kafkaReader.FetchMessage(ctx)
		if err != nil {
			if err == ctx.Err() {
				return kafkaMsg, true
			}
			logx.Error(err)
			if fetchRetryCnt++; fetchRetryCnt >= 3 {
				return kafkaMsg, true
			}
			time.Sleep(1 * time.Second)
			continue fetch
		}
		break fetch
	}
	return kafkaMsg, false
}
