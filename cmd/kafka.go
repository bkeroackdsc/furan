package cmd

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/bsm/sarama-cluster"
	"github.com/gocql/gocql"
	"github.com/golang/protobuf/proto"
)

const (
	maxFlushMsgs       = 5
	maxFlushFreqSecs   = 1
	connectTimeoutSecs = 10
	keepaliveSecs      = 5
)

type kafkaconfig struct {
	brokers      []string
	topic        string
	manager      EventBusManager
	maxOpenSends uint
}

func setupKafka() {
	kafkaConfig.brokers = strings.Split(kafkaBrokerStr, ",")
	if len(kafkaConfig.brokers) < 1 {
		log.Fatalf("At least one Kafka broker is required")
	}
	if kafkaConfig.topic == "" {
		log.Fatalf("Kafka topic is required")
	}
	kp, err := NewKafkaManager(kafkaConfig.brokers, kafkaConfig.topic, kafkaConfig.maxOpenSends, logger)
	if err != nil {
		log.Fatalf("Error creating Kafka producer: %v", err)
	}
	kafkaConfig.manager = kp
}

// EventBusProducer describes an object capable of publishing events somewhere
type EventBusProducer interface {
	PublishEvent(*BuildEvent) error
}

// EventBusConsumer describes an object cabable of subscribing to events somewhere
type EventBusConsumer interface {
	SubscribeToTopic(chan<- *BuildEvent, <-chan struct{}, gocql.UUID) error
}

// EventBusManager describes an object that can publish and subscribe to events somewhere
type EventBusManager interface {
	EventBusProducer
	EventBusConsumer
}

// KafkaManager handles sending event messages to the configured Kafka topic
type KafkaManager struct {
	ap           sarama.AsyncProducer
	topic        string
	brokers      []string
	consumerConf *cluster.Config
	logger       *log.Logger
}

// NewKafkaManager returns a new Kafka manager object
func NewKafkaManager(brokers []string, topic string, maxsends uint, logger *log.Logger) (*KafkaManager, error) {
	pconf := sarama.NewConfig()

	pconf.Net.MaxOpenRequests = int(maxsends)
	pconf.Net.DialTimeout = connTimeoutSecs * time.Second
	pconf.Net.ReadTimeout = connTimeoutSecs * time.Second
	pconf.Net.WriteTimeout = connTimeoutSecs * time.Second
	pconf.Net.KeepAlive = keepaliveSecs * time.Second

	pconf.Producer.Return.Errors = true
	pconf.Producer.Flush.Messages = maxFlushMsgs
	pconf.Producer.Flush.Frequency = maxFlushFreqSecs * time.Second

	asyncp, err := sarama.NewAsyncProducer(brokers, pconf)
	if err != nil {
		return nil, err
	}

	cconf := cluster.NewConfig()
	cconf.Net = pconf.Net
	cconf.Consumer.Return.Errors = true

	kp := &KafkaManager{
		ap:           asyncp,
		topic:        topic,
		brokers:      kafkaConfig.brokers,
		consumerConf: cconf,
		logger:       logger,
	}
	go kp.handlePErrors()
	return kp, nil
}

func (kp *KafkaManager) handlePErrors() {
	var kerr *sarama.ProducerError
	for {
		kerr = <-kp.ap.Errors()
		log.Printf("Kafka producer error: %v", kerr)
	}
}

// PublishEvent publishes a build event to the configured Kafka topic
func (kp *KafkaManager) PublishEvent(event *BuildEvent) error {
	id, err := gocql.ParseUUID(event.BuildId)
	if err != nil {
		return err
	}
	val, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("error marshaling protobuf: %v", err)
	}
	pmsg := &sarama.ProducerMessage{
		Topic: kp.topic,
		Key:   sarama.ByteEncoder(id.Bytes()), // Key is build ID to preserve event order (all events of a build go to the same partition)
		Value: sarama.ByteEncoder(val),
	}
	select { // don't block if Kafka is unavailable for some reason
	case kp.ap.Input() <- pmsg:
		return nil
	default:
		return fmt.Errorf("could not publish Kafka message: channel full")
	}
}

// SubscribeToTopic listens to the configured topic, filters by build_id and writes
// the resulting messages to output. When the subscribed build is finished
// output is closed. done is a signal from the caller to abort the stream subscription
func (kp *KafkaManager) SubscribeToTopic(output chan<- *BuildEvent, done <-chan struct{}, buildID gocql.UUID) error {
	// random group ID for each connection
	groupid, err := gocql.RandomUUID()
	if err != nil {
		return err
	}
	con, err := cluster.NewConsumer(kp.brokers, groupid.String(), []string{kp.topic}, kp.consumerConf)
	if err != nil {
		return err
	}
	handleConsumerErrors := func() {
		var err error
		for {
			err = <-con.Errors()
			if err == nil { // chan closed
				return
			}
			kp.logger.Printf("Kafka consumer error: %v", err)
		}
	}
	go handleConsumerErrors()
	go func() {
		defer close(output)
		defer con.Close()
		var err error
		var msg *sarama.ConsumerMessage
		var event *BuildEvent
		input := con.Messages()
		for {
			select {
			case <-done:
				kp.logger.Printf("SubscribeToTopic: aborting")
				return
			default:
				break
			}
			msg = <-input
			if msg == nil {
				return
			}
			if bytes.Equal(msg.Key, []byte(buildID[:])) {
				event = &BuildEvent{}
				err = proto.Unmarshal(msg.Value, event)
				if err != nil {
					kp.logger.Printf("%v: error unmarshaling event from Kafka stream: %v", buildID.String(), err)
					continue
				}
				output <- event
				if event.BuildFinished || event.EventError.IsError {
					return
				}
			}
		}
	}()
	return nil
}
