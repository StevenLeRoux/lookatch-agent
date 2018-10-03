package sinks

import (
	"encoding/json"
	"time"

	"encoding/binary"
	"github.com/Pirionfr/lookatch-common/events"
	"github.com/Pirionfr/lookatch-common/util"
	"github.com/Shopify/sarama"
	"github.com/juju/errors"
	log "github.com/sirupsen/logrus"
)

// KafkaType type of sink
const KafkaType = "kafka"

type (
	// kafkaUser representation of kafka User
	kafkaUser struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}

	// kafkaSinkConfig representation of kafka sink config
	kafkaSinkConfig struct {
		Tls             bool       `json:"tls"`
		Topic           string     `json:"topic"`
		Topic_prefix    string     `json:"topic_prefix"`
		Client_id       string     `json:"client_id"`
		Brokers         []string   `json:"brokers"`
		Producer        *kafkaUser `json:"producer"`
		Consumer        *kafkaUser `json:"consumer"`
		MaxMessageBytes int        `json:"maxmessagebytes"`
		NbProducer      int        `json:"nbproducer"`
		Secret          string     `json:"secret"`
	}

	// Kafka representation of kafka sink
	Kafka struct {
		*Sink
		kafkaConf *kafkaSinkConfig
	}
)

// newKafka create new kafka sink
func newKafka(s *Sink) (SinkI, error) {

	ksConf := &kafkaSinkConfig{}
	s.conf.Unmarshal(ksConf)

	return &Kafka{
		Sink:      s,
		kafkaConf: ksConf,
	}, nil
}

// Start kafka sink
func (k *Kafka) Start(_ ...interface{}) error {

	resendChan := make(chan *sarama.ProducerMessage, 10000)
	// Notice order could get altered having more than 1 producer
	log.WithFields(log.Fields{
		"NbProducer": k.kafkaConf.NbProducer,
	}).Debug("Starting sink producers")
	for x := 0; x < k.kafkaConf.NbProducer; x++ {
		go startProducer(k.kafkaConf, resendChan, k.stop)
	}

	//current kafka threshold is 10MB
	threshold := k.kafkaConf.MaxMessageBytes
	log.WithFields(log.Fields{
		"threshold": threshold,
	}).Debug("KafkaSink: started with threshold")

	go startConsumer(k.kafkaConf, k.in, threshold, resendChan)

	return nil
}

//GetInputChan return input channel attach to sink
func (k *Kafka) GetInputChan() chan *events.LookatchEvent {
	return k.in
}

// startConsumer consume input chan
func startConsumer(conf *kafkaSinkConfig, input chan *events.LookatchEvent, threshold int, kafkaChan chan *sarama.ProducerMessage) {
	for {
		for eventMsg := range input {

			//id event is too heavy it wont fit in kafka threshold so we have to skip it
			switch typedMsg := eventMsg.Payload.(type) {
			case *events.SqlEvent:
				producerMsg, err := processSQLEvent(typedMsg, conf, threshold)
				if err != nil {
					break
				}
				kafkaChan <- producerMsg
			case *events.GenericEvent:
				producerMsg, err := processGenericEvent(typedMsg, conf, threshold)
				if err != nil {
					break
				}
				kafkaChan <- producerMsg
			case *sarama.ConsumerMessage:
				producerMsg, err := processKafkaMsg(typedMsg, conf, threshold)
				if err != nil {
					break
				}
				kafkaChan <- producerMsg
			default:
				log.WithFields(log.Fields{
					"message": eventMsg,
				}).Debug("KafkaSink: event doesn't match any known type: ", eventMsg)
			}
		}
	}
}

// processGenericEvent process Generic Event
func processGenericEvent(genericMsg *events.GenericEvent, conf *kafkaSinkConfig, threshold int) (*sarama.ProducerMessage, error) {
	var topic string
	if len(conf.Topic) == 0 {
		topic = conf.Topic_prefix + genericMsg.Environment
	} else {
		topic = conf.Topic
	}
	var msgToSend []byte
	serializedEventPayload, err := json.Marshal(genericMsg)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("KafkaSink Marshal Error")
	}
	if len(conf.Secret) > 0 {
		var err error
		msgToSend, err = util.EncryptBytes(serializedEventPayload, conf.Secret)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("KafkaSink Encrypt Error")
		}
	} else {
		msgToSend = serializedEventPayload
	}
	//if message is heavier than threshold we must skip it
	if len(msgToSend) > threshold {
		errMsg := "KafkaSink: Skip too heavy event : "
		log.WithFields(log.Fields{
			"size":      len(msgToSend),
			"threshold": threshold,
			"topic":     topic,
		}).Debug("KafkaSink: Skip too heavy event")
		return nil, errors.New(errMsg)
	}

	log.WithFields(log.Fields{
		"topic": topic,
	}).Debug("KafkaSink: sending to topic")
	return &sarama.ProducerMessage{Topic: topic, Key: sarama.ByteEncoder(genericMsg.Environment), Value: sarama.StringEncoder(msgToSend)}, nil

}

// processSQLEvent process Sql Event
func processSQLEvent(sqlEvent *events.SqlEvent, conf *kafkaSinkConfig, threshold int) (*sarama.ProducerMessage, error) {
	var topic string
	if len(conf.Topic) == 0 {
		topic = conf.Topic_prefix + sqlEvent.Environment + "_" + sqlEvent.Database
	} else {
		topic = conf.Topic
	}
	log.WithFields(log.Fields{
		"topic": topic,
	}).Debug("KafkaSink: Sending event to topic")
	key := sqlEvent.PrimaryKey
	serializedEventPayload, err := json.Marshal(sqlEvent)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("KafkaSink Marshal Error")
	}
	if len(conf.Secret) > 0 {

		result, err := util.EncryptString(string(serializedEventPayload[:]), conf.Secret)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("KafkaSink Encrypt Error")
		}
		serializedEventPayload = []byte(result)
	}
	//if message is heavier than threshold we must skip it
	if len(serializedEventPayload) > threshold {
		errMsg := "KafkaSink: Skip too heavy event"
		log.WithFields(log.Fields{
			"size":      len(serializedEventPayload),
			"threshold": threshold,
			"event":     sqlEvent.Database + "." + sqlEvent.Table,
		}).Debug(errMsg)
		return nil, errors.New(errMsg)
	}

	return &sarama.ProducerMessage{Topic: topic, Key: sarama.StringEncoder(key), Value: sarama.StringEncoder(string(serializedEventPayload))}, nil
}

// processKafkaMsg process Kafka Msg
func processKafkaMsg(kafkaMsg *sarama.ConsumerMessage, conf *kafkaSinkConfig, threshold int) (*sarama.ProducerMessage, error) {
	log.WithFields(log.Fields{
		"topic": kafkaMsg.Topic,
		"Value": kafkaMsg.Value,
	}).Debug("KafkaSink: incoming Msg")

	var topic string
	if len(conf.Topic) == 0 {
		topic = conf.Topic_prefix + kafkaMsg.Topic
	} else {
		topic = conf.Topic
	}
	var msgToSend []byte
	if conf.Secret != "" {
		var err error
		msgToSend, err = util.EncryptBytes(kafkaMsg.Value, conf.Secret)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("KafkaSink Encrypt Error: ")
		}
	} else {
		msgToSend = kafkaMsg.Value
	}
	//if message is heavier than threshold we must skip it
	if len(msgToSend) > threshold {
		errMsg := "KafkaSink: Skip too heavy event"
		log.WithFields(log.Fields{
			"size":      len(msgToSend),
			"threshold": threshold,
			"topic":     kafkaMsg.Topic,
		}).Debug(errMsg)
		return nil, errors.New(errMsg)
	}

	return &sarama.ProducerMessage{Topic: topic, Key: sarama.ByteEncoder(kafkaMsg.Key), Value: sarama.StringEncoder(msgToSend)}, nil
}

// startProducer send message to kafka
func startProducer(conf *kafkaSinkConfig, in chan *sarama.ProducerMessage, stop chan error) {

	saramaConf := sarama.NewConfig()
	saramaConf.Producer.Retry.Max = 5
	saramaConf.Producer.Return.Successes = true
	saramaConf.Producer.MaxMessageBytes = conf.MaxMessageBytes

	if len(conf.Client_id) == 0 {
		log.Debug("No client id")
		saramaConf.Net.SASL.Enable = true
		log.Debug("SASL CLient ")
		if conf.Tls {
			log.Debug("TLS connection ")
			saramaConf.Net.TLS.Enable = conf.Tls
		}
		saramaConf.Net.SASL.User = conf.Producer.User
		saramaConf.Net.SASL.Password = conf.Producer.Password
	} else {
		saramaConf.ClientID = conf.Client_id
		log.WithFields(log.Fields{
			"clientID": saramaConf.ClientID,
		}).Debug("sink_conf sarama_conf ")
	}

	if err := saramaConf.Validate(); err != nil {
		errMsg := "startProducer: sarama configuration not valid : "
		stop <- errors.Annotate(err, errMsg)
	}
	producer, err := sarama.NewSyncProducer(conf.Brokers, saramaConf)
	if err != nil {
		errMsg := "Error when Initialize NewSyncProducer"
		stop <- errors.Annotate(err, errMsg)
	}

	log.Debug("startProducer: New SyncProducer created")

	defer func() {
		if err := producer.Close(); err != nil {
			stop <- errors.Annotate(err, "Error while closing kafka producer")
		}
		log.Debug("Successfully Closed kafka producer")
	}()

	//log.Println("DEBUG: eventProducer spawning loop")
	var (
		enqueued           int
		msg                *sarama.ProducerMessage
		msgs               []*sarama.ProducerMessage
		lastSend, timepass int64
		msgsSize, msgSize  int
	)
	lastSend = time.Now().Unix()
ProducerLoop:
	for {
		select {
		case msg = <-in:
			if msg.Value.Length() == 0 {
				log.Debug("Receive empty Path")
				if len(msgs) >= 1 {
					lastSend = sendMsg(msgs, producer)
					msgs = []*sarama.ProducerMessage{}
					msgsSize = 0
				}
			} else {

				//calcul size
				msgSize = msgByteSize(msg)
				if msgSize > conf.MaxMessageBytes {
					log.Warn("Skip Message")

				} else if msgsSize+msgSize < conf.MaxMessageBytes {
					msgs = append(msgs, msg)
					msgsSize += msgSize
				} else {
					lastSend = sendMsg(msgs, producer)
					msgs = []*sarama.ProducerMessage{}
					msgs = append(msgs, msg)
					msgsSize = msgSize
				}

				//use to clear slice
				timepass = time.Now().Unix() - lastSend
				if timepass >= 1 {
					lastSend = sendMsg(msgs, producer)
					msgs = []*sarama.ProducerMessage{}
					msgsSize = 0
				}
			}
		case <-stop:
			log.Info("startProducer: Signal received, closing producer")
			break ProducerLoop
		}
	}
	log.WithFields(log.Fields{
		"Enqueued": enqueued,
	}).Info("startProducer")
}

func sendMsg(msgs []*sarama.ProducerMessage, producer sarama.SyncProducer) int64 {
	retries := 0
	err := producer.SendMessages(msgs)
	for err != nil {
		producerErrs := err.(sarama.ProducerErrors)
		msgs = []*sarama.ProducerMessage{}
		for _, v := range producerErrs {
			log.WithFields(log.Fields{
				"error": v.Err,
			}).Warn("failed to push to kafka")
			msgs = append(msgs, v.Msg)
		}

		if retries > 20 {
			log.WithFields(log.Fields{
				"nbRetry": retries,
			}).Panic("Failed to push event to kafka. Stopping agent.")
		}
		retries++
		err = producer.SendMessages(msgs)
	}
	return time.Now().Unix()
}

func msgByteSize(msg *sarama.ProducerMessage) int {
	// the metadata overhead of CRC, flags, etc.
	size := 26
	for _, h := range msg.Headers {
		size += len(h.Key) + len(h.Value) + 2*binary.MaxVarintLen32
	}
	if msg.Key != nil {
		size += msg.Key.Length()
	}
	if msg.Value != nil {
		size += msg.Value.Length()
	}
	return size
}
