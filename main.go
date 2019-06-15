package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/facebookgo/pidfile"
	uuid "github.com/satori/go.uuid"
	"github.com/streadway/amqp"
	"gopkg.in/natefinch/lumberjack.v2"
)

// amqp connect url
var AmqpUrl string

const (
	// chan
	ChannelBufferLength = 100

	//worker number
	ReceiverNum = 5
	WorkerNum   = 10
	AckerNum    = 10
	ResenderNum = 5

	// http tune
	HttpMaxIdleConns        = 500 // default 100 in net/http
	HttpMaxIdleConnsPerHost = 500 // default 2 in net/http
	HttpIdleConnTimeout     = 30  // default 90 in net/http

	// reconnect to rabbit for connection interrupt
	ReconnectRabbitIntervalSeconds = 1
	ReconnectRabbitTimes           = 10
)

func receiveMessage(queues []*QueueConfig, done <-chan struct{}) <-chan Message {
	out := make(chan Message, ChannelBufferLength)
	var wg sync.WaitGroup

	receiver := func(qc QueueConfig) {
		defer wg.Done()

	RECONNECT:
		for {
			_, channel, err := setupChannel()
			if err != nil {
				PanicOnError(err)
			}

			msgs, err := channel.Consume(
				qc.WorkerQueueName(), // queue
				"",                   // consumer
				false,                // auto-ack
				false,                // exclusive
				false,                // no-local
				false,                // no-wait
				nil,                  // args
			)
			PanicOnError(err)

			for {
				select {
				case msg, ok := <-msgs:
					if !ok {
						log.Printf("receiver: channel is closed, maybe lost connection")
						time.Sleep(5 * time.Second)
						continue RECONNECT
					}
					// u, _ := uuid.NewV4()
					// msg.MessageId = fmt.Sprintf("%s", u.String())
					msg.MessageId = fmt.Sprintf("%s", uuid.NewV4())
					message := Message{qc, &msg, 0}
					out <- message

					message.Printf("receiver: received msg")
				case <-done:
					log.Printf("receiver: received a done signal")
					return
				}
			}
		}
	}

	for _, queue := range queues {
		wg.Add(ReceiverNum)
		for i := 0; i < ReceiverNum; i++ {
			go receiver(*queue)
		}
	}

	go func() {
		wg.Wait()
		log.Printf("all receiver is done, closing channel")
		close(out)
	}()

	return out
}

func workMessage(in <-chan Message) <-chan Message {
	var wg sync.WaitGroup
	out := make(chan Message, ChannelBufferLength)
	client := newHttpClient(HttpMaxIdleConns, HttpMaxIdleConnsPerHost, HttpIdleConnTimeout)

	worker := func(m Message, o chan<- Message) {
		m.Printf("worker: received a msg, body: %s", string(m.amqpDelivery.Body))

		defer wg.Done()
		m.Notify(client)
		o <- m
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		for message := range in {
			wg.Add(1)
			go worker(message, out)
		}
	}()

	go func() {
		wg.Wait()
		log.Printf("all worker is done, closing channel")
		close(out)
	}()

	return out
}

func ackMessage(in <-chan Message) <-chan Message {
	out := make(chan Message)
	var wg sync.WaitGroup

	acker := func() {
		defer wg.Done()

		for m := range in {
			m.Printf("acker: received a msg")

			if m.IsNotifySuccess() {
				m.Ack()
			} else if m.IsMaxRetry() {
				m.Republish(out)
			} else {
				m.Reject()
			}
		}
	}

	for i := 0; i < AckerNum; i++ {
		wg.Add(1)
		go acker()
	}

	go func() {
		wg.Wait()
		log.Printf("all acker is done, close out")
		close(out)
	}()

	return out
}

func resendMessage(in <-chan Message) <-chan Message {
	out := make(chan Message)

	var wg sync.WaitGroup

	resender := func() {
		defer wg.Done()

	RECONNECT:
		for {
			conn, channel, err := setupChannel()
			if err != nil {
				PanicOnError(err)
			}

			for m := range in {
				err := m.CloneAndPublish(channel)
				if err == amqp.ErrClosed {
					time.Sleep(5 * time.Second)
					continue RECONNECT
				}
			}

			// normally quit , we quit too
			conn.Close()
			break
		}
	}

	for i := 0; i < ResenderNum; i++ {
		wg.Add(1)
		go resender()
	}

	go func() {
		wg.Wait()
		log.Printf("all resender is done, close out")
		close(out)
	}()

	return out
}

func handleSignal(done chan<- struct{}) {
	chan_sigs := make(chan os.Signal, 1)
	signal.Notify(chan_sigs, syscall.SIGQUIT)

	go func() {
		sig := <-chan_sigs

		if sig != nil {
			log.Printf("received a signal %v, close done channel", sig)
			close(done)
		}
	}()
}

func main() {
	// parse command line args
	configFileName := flag.String("c", "", "config file path")
	logFileName := flag.String("log", "", "logging file, default STDOUT")

	// log
	logMaxSizeMB := flag.Int("log_size", 500, "log maximum size MB, default 500")
	logBackups := flag.Int("log_backups", 15, "log backup counts, default 15")
	logMaxAgeDay := flag.Int("log_max_age", 15, "log maximum retention day, default 15")
	logIsCompress := flag.Bool("log_compress", true, "if compress log, using gzip, default true")
	flag.StringVar(&AmqpUrl, "amqp_url", "", "rabbit mq url")
	flag.Parse()

	// write pid file
	pidfile.Write()

	// set loger
	if *logFileName != "" {
		f, err := os.OpenFile(*logFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		PanicOnError(err)
		defer f.Close()

		log.SetOutput(&lumberjack.Logger{
			Filename:   *logFileName,
			MaxSize:    *logMaxSizeMB,
			MaxBackups: *logBackups,
			MaxAge:     *logMaxAgeDay,
			LocalTime:  true,
			Compress:   *logIsCompress,
		})
	}

	// read yaml config
	var allQueues []*QueueConfig
	allQueues = loadQueuesConfig(*configFileName, allQueues)

	// create queues
	_, channel, err := setupChannel()
	if err != nil {
		PanicOnError(err)
	}
	for _, queue := range allQueues {
		log.Printf("allQueues: queue config: %v", queue)
		queue.DeclareExchange(channel)
		queue.DeclareQueue(channel)
	}

	// register signal
	done := make(chan struct{}, 1)
	handleSignal(done)

	// change gorouting config
	log.Printf("set gorouting to the number of logical CPU: %d", runtime.NumCPU())
	runtime.GOMAXPROCS(runtime.NumCPU())

	// main logic
	<-resendMessage(ackMessage(workMessage(receiveMessage(allQueues, done))))

	log.Printf("exiting program")
}
