package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"runtime"

	"strings"

	"io/ioutil"
	"sync"

	"github.com/Jeffail/gabs"
	logger "github.com/snail007/mini-logger"
	"github.com/streadway/amqp"
	"github.com/valyala/fasthttp"
)

type message struct {
	Consumers   []consumer
	Durable     bool
	IsNeedToken bool
	Mode        string
	Name        string
	Token       string
	Comment     string
}
type consumer struct {
	ID        string
	URL       string
	RouteKey  string
	Timeout   float64
	Code      float64
	CheckCode bool
	Comment   string
}

var (
	msgLock = &sync.Mutex{}
)

func parseMessages(str string) (messages []message, err error) {
	defer func(err *error) {
		e := recover()
		if e != nil {
			*err = fmt.Errorf("%s", e)
		}
	}(&err)
	jsonData, _ := gabs.ParseJSON([]byte(str))
	msgs, _ := jsonData.Children()
	for _, m := range msgs {
		msg := message{}
		json.Unmarshal(m.Bytes(), &msg)
		messages = append(messages, msg)
	}
	return
}
func messageIsExists(name string) bool {
	for _, m := range messages {
		if m.Name == name {
			return true
		}
	}
	return false
}
func consumerIsExists(exchangeName, consumerName string) (exists bool, exchangeIndex, consumerIndex int) {
	for k1, m := range messages {
		if m.Name == exchangeName {
			for k2, c := range m.Consumers {
				if c.ID == consumerName {
					return true, k1, k2
				}
			}
		}
	}
	return false, 0, 0
}
func getMessage(name string) (msg *message, key int, err error) {
	for k, m := range messages {
		if m.Name == name {
			msg = &m
			key = k
			return
		}
	}
	err = errors.New("message not found")
	return
}
func getConsumer(msgMame, consumerID string) (consumer *consumer, msgIndex, consumerIndex int, err error) {
	_, i, e := getMessage(msgMame)
	if e != nil {
		err = e
		return
	}
	msgIndex = i
	for k, c := range messages[i].Consumers {
		if c.ID == consumerID {
			consumer = &c
			consumerIndex = k
			return
		}
	}
	err = errors.New("consumer not found")
	return
}
func config() (jsonData string, err error) {
	msgLock.Lock()
	defer msgLock.Unlock()
	j, err := json.Marshal(messages)
	jsonData = string(j)
	return
}
func statusMessage(messageName string) (jsonData string, err error) {
	m, _, err := getMessage(messageName)
	if err != nil {
		return
	}
	var ja []string

	for _, c := range m.Consumers {
		j, _ := statusConsumer(m.Name, c.ID)
		ja = append(ja, j.String())
	}
	jsonData = "[" + strings.Join(ja, ",") + "]"
	return
}
func statusConsumer(messageName, consumerID string) (json *gabs.Container, err error) {
	msgLock.Lock()
	defer msgLock.Unlock()
	c, i, _, e := getConsumer(messageName, consumerID)
	if e != nil {
		return nil, e
	}
	m := messages[i]
	var q amqp.Queue
	//var ch *amqp.Channel
	//ch, _ = getMqChannel()
	//q, e = ch.QueueDeclare(getConsumerKey(m, *c), m.Durable, !m.Durable, false, false, nil)
	//defer channelPools.Put(ch)
	q, _, e = queueDeclare(getConsumerKey(m, *c), m.Durable)
	if e != nil {
		return nil, e
	}
	count := q.Messages
	var jsonObj = gabs.New()
	jsonObj.Set(count, "Count")
	jsonObj.Set(consumerID, "ID")
	jsonObj.Set(messageName, "MsgName")
	jsonObj.Set("0", "LastTime")
	lasttime, e := statusConsumerWorker(*c, m)
	if e == nil {
		jsonObj.Set(lasttime, "LastTime")
	}
	//jsonObj.StringIndent("", "  ")
	//json = jsonObj.String()
	json = jsonObj
	return
}
func addMessage(m message) (err error) {
	ctx := ctxFunc("addMessage")
	msgLock.Lock()
	defer msgLock.Unlock()
	messages = append(messages, m)
	ctx.With(logger.Fields{"message": m.Name}).Infof("was added")
	initMessages()
	return nil
}
func updateMessage(m message) (err error) {
	ctx := ctxFunc("updateMessage")
	msgLock.Lock()
	defer msgLock.Unlock()
	_, i, e := getMessage(m.Name)
	if e != nil {
		err = e
		return
	}
	m.Consumers = messages[i].Consumers
	messages[i] = m
	for _, c := range m.Consumers {
		_, e := stopConsumerWorker(c, m)
		if e != nil {
			err = e
			ctx.With(logger.Fields{"message": m.Name, "consumer": c.ID, "call": "stopConsumerWorker"}).Infof("fail , %s", e)
			return
		}
	}
	ctx.Infof("updated")
	initMessages()
	return
}
func deleteMessage(m message) (err error) {
	ctx := ctxFunc("deleteMessage").With(logger.Fields{"message": m.Name})
	msgLock.Lock()
	defer msgLock.Unlock()
	msg, i, e := getMessage(m.Name)
	if e != nil {
		err = e
		return
	}
	//stop all consumer worker
	for _, c := range msg.Consumers {
		_, e := stopConsumerWorker(c, m)
		if e != nil {
			err = e
			ctx.With(logger.Fields{"call": "stopConsumerWorker"}).Infof("delete fail ,%s", e)
			return
		}
		//delete queue
		er := deleteQueue(getConsumerKey(*msg, c))
		if er != nil {
			err = er
			ctx.With(logger.Fields{"call": "deleteQueue"}).Infof("delete fail, %s", e)
			return
		}
	}
	//delete exchange
	err = deleteExchange(msg.Name)
	if err != nil {
		ctx.With(logger.Fields{"deleteExchange": msg.Name}).Warnf("delete fail,ERR:%s", err)
		return
	}
	//update messsages data
	messages = append(messages[:i], messages[i+1:]...)
	ctx.Infof("deleted")
	initMessages()
	return
}
func addConsumer(msg message, c0 consumer) (err error) {
	ctx := ctxFunc("addConsumer")
	msgLock.Lock()
	defer msgLock.Unlock()
	//add it to messages
	var i int
	_, i, err = getMessage(msg.Name)
	if err != nil {
		return
	}
	//update messages data
	messages[i].Consumers = append(messages[i].Consumers, c0)
	//update worker
	initMessages()
	ctx.With(logger.Fields{"consumer": getConsumerKey(msg, c0)}).Infof("added")
	return
}

func updateConsumer(msg message, c0 consumer) (err error) {
	msgLock.Lock()
	defer msgLock.Unlock()
	_, i, k, e := getConsumer(msg.Name, c0.ID)
	if e != nil {
		return e
	}
	//update messages data
	messages[i].Consumers[k] = c0

	//update consumer worker
	_, err = updateConsumerWorker(c0, msg)
	return
}

func deleteConsumer(msg message, c0 consumer) (err error) {
	ctx := ctxFunc("deleteConsumer")
	msgLock.Lock()
	defer msgLock.Unlock()
	_, i0, i, err := getConsumer(msg.Name, c0.ID)
	if err != nil {
		ctx.With(logger.Fields{"consumer": getConsumerKey(msg, c0)}).Warnf("delete fail,ERR:%s", err)
		return
	}
	//delete messages consumer
	messages[i0].Consumers = append(messages[i0].Consumers[:i], messages[i0].Consumers[i+1:]...)
	//stop consumer
	_, err = stopConsumerWorker(c0, msg)
	if err != nil {
		ctx.With(logger.Fields{"consumer": getConsumerKey(msg, c0)}).Warnf("delete fail,ERR:%s", err)
		return
	}
	//update messages worker
	err = initMessages()
	if err != nil {
		ctx.With(logger.Fields{"consumer": getConsumerKey(msg, c0)}).Warnf("delete fail,ERR:%s", err)
		return
	}
	//delete queue
	err = deleteQueue(getConsumerKey(msg, c0))
	if err != nil {
		ctx.With(logger.Fields{"deleteQueue": getConsumerKey(msg, c0)}).Warnf("delete fail,ERR:%s", err)
		return
	}
	ctx.With(logger.Fields{"consumer": getConsumerKey(msg, c0)}).Infof("deleted")
	return
}

func publish(body, exchangeName, routeKey, token string) (err error) {
	ctx := ctxFunc("publish")
	var msg *message
	msg, _, err = getMessage(exchangeName)
	if err != nil {
		return
	}
	if msg.IsNeedToken && token != msg.Token {
		err = errors.New("token error")
		return
	}
	var channel *amqp.Channel
	channel, err = getMqChannel()
	if err == nil {
		err = channel.Publish(getExchangeName(exchangeName), routeKey, false, false, amqp.Publishing{
			Body: []byte(body),
		})
		channelPools.Put(channel)
		ctx1 := ctx.With(logger.Fields{"call": "channel.Publish", "exchange": getExchangeName(exchangeName)})
		if err == nil {
			ctx1.Debugf("success")
			return
		}
		ctx1.Warnf("publish fail,%s", err)
	} else {
		ctx.With(logger.Fields{"call": "getMqChannel"}).Warnf("fail,%s", err)
	}
	return
}

//WrapedConsumer xx
type wrapedConsumer struct {
	consumer  consumer
	message   message
	action    string
	lasttime  int64
	readChan  <-chan string
	writeChan chan<- string
}

type manageConsumer struct {
	wrapedConsumer
	consumerReadChan  chan string
	consumerWriteChan chan string
	key               string
}

var consumerManageReadChan, consumerManageWriteChan = make(chan wrapedConsumer, 1), make(chan string, 1)

func updateConsumerWorker(c consumer, m message) (answer string, err error) {
	return notifyConsumerManager("update", c, m)
}
func stopConsumerWorker(c consumer, m message) (answer string, err error) {
	return notifyConsumerManager("delete", c, m)
}
func statusConsumerWorker(c consumer, m message) (answer string, err error) {
	return notifyConsumerManager("status", c, m)
}
func notifyConsumerManager(action string, c consumer, m message) (answer string, err error) {
	if action != "update" && action != "delete" && action != "status" {
		err = fmt.Errorf("action must be [update|delete]")
		return
	}
	ctx := ctxFunc("notifyConsumerManager").With(logger.Fields{"action": action, "message": "m.Name", "consumer": c.ID})
	ctx.Debugf("sending...")
	consumerManageReadChan <- wrapedConsumer{
		action:   action,
		consumer: c,
		message:  m,
	}
	ctx.Debugf("response to consumer manager...")
	answer = <-consumerManageWriteChan
	ctx.Debugf("consumer manager answer : %s", answer)
	return
}
func getConsumerKey(m message, c consumer) string {
	return m.Name + "-" + c.ID
}
func initMessages() (err error) {
	ctx := ctxFunc("initMessages")
	answer := ""
	for _, m := range messages {
		_, err = exchangeDeclare(m.Name, m.Mode, m.Durable)
		ctx1 := ctx.With(logger.Fields{"exchange": m.Name})
		if err != nil {
			ctx1.Warnf("declare fail , %s ", err)
			return
		}
		for _, c := range m.Consumers {
			_, _, err = queueDeclare(getConsumerKey(m, c), m.Durable)
			ctx2 := ctx1.With(logger.Fields{"queue": getConsumerKey(m, c)})
			if err != nil {
				ctx2.Warnf("declare fail , %s ", err)
				return
			}
			err = queueBindToExchange(getConsumerKey(m, c), m.Name, c.RouteKey)
			if err != nil {
				ctx2.Warnf("bind fail , %s ", err)
				return
			}
			answer, err = updateConsumerWorker(c, m)
			ctx3 := ctx2.With(logger.Fields{"call": "updateConsumerWorker"})
			if err != nil {
				ctx3.Warnf("%s ", err)
				return
			}
			ctx3.Debugf("answer %s ", answer)
		}
	}
	return
}
func restart() (err error) {
	msgLock.Lock()
	defer msgLock.Unlock()
	err = stopAllConsumer()
	if err != nil {
		return
	}
	initMessages()
	return
}
func reload() {
	msgLock.Lock()
	defer msgLock.Unlock()
	initMessages()
}
func stopAllConsumer() (err error) {
	for _, m := range messages {
		for _, c := range m.Consumers {
			_, err = stopConsumerWorker(c, m)
			if err != nil {
				return
			}
		}
	}
	return
}
func initConsumerManager() {
	ctx := ctxFunc("initConsumerManager")
	go func() {
		ctx.Debugf("started")
		//wrapedConsumers := make(map[string]manageConsumer)
		wrapedConsumers := NewConcurrentMap()
		waitSeconds := time.Duration(cfg.GetInt("consume.GoFailWait"))
		waitSeconds1 := time.Duration(cfg.GetInt("consume.FailWait"))
		errStr := fmt.Sprintf("fail,sleep %d seconds ... ", waitSeconds)
		//Consumer Manager worker loop,waiting for control command and exec switch
		for {
			//waiting for control data
			wrapedConsumer := <-consumerManageReadChan
			key := getConsumerKey(wrapedConsumer.message, wrapedConsumer.consumer)
			//preprocess for control data

			ctx1 := ctx.With(logger.Fields{"wrapedConsumer": key})
			ctx1.Debugf("revecived")
			//decide what to do
			switch wrapedConsumer.action {
			case "update": //update or insert consumer
				t := "update"
				if item, ok := wrapedConsumers.Get(key); !ok {
					t = "insert"
				} else {
					_item := item.(manageConsumer)
					_item.lasttime = time.Now().Unix()
					wrapedConsumers.Set(key, _item)
				}
				ctx1.Debugf("%s", t)
				wrapedConsumers.Set(key, manageConsumer{
					wrapedConsumer:    wrapedConsumer,
					consumerReadChan:  make(chan string, 1),
					consumerWriteChan: make(chan string, 1),
					key:               key,
				})
				if t == "insert" {
					//start consumer go
					go func() {
						var channel *amqp.Channel
						defer func() {
							wrapedConsumers.Remove(key)
							if channel != nil {
								channel.Close()
								ctx1.Warnf("channel %s closed ", key)
							}
							ctx1.Warnf("goroutine exited")
						}()
						//0.consumer worker start
						for {
						RETRY:
							//1.check if exists in wrapedConsumers data
							if _, ok := wrapedConsumers.Get(key); !ok {
								ctx1.Warnf("not found , now exit")
								runtime.Goexit()
							}
							//update consumer active time
							item, _ := wrapedConsumers.Get(key)
							_item := item.(manageConsumer)
							_item.lasttime = time.Now().Unix()
							wrapedConsumers.Set(key, _item)
							//2.try get connection
							conn, err := pools.Get()
							if err != nil {
								pools.Put(conn)
								ctx1.With(logger.Fields{"call": "pools.Get"}).Warnf(errStr+"%s", err)
								time.Sleep(time.Second * waitSeconds)
								continue
							}
							//3.try get channel on connecton
							channel, err = conn.(*amqp.Connection).Channel()
							if err != nil {
								pools.Put(conn)
								ctx1.With(logger.Fields{"call": "pools.Put"}).Warnf(errStr+"%s", err)
								time.Sleep(time.Second * waitSeconds)
								continue
							}
							//4.try  declare exchange  on channel
							item, _ = wrapedConsumers.Get(key)
							_item = item.(manageConsumer)
							_, err = exchangeDeclare(_item.message.Name,
								_item.message.Mode,
								_item.message.Durable)
							if err != nil {
								pools.Put(conn)
								ctx1.With(logger.Fields{"call": "exchangeDeclare"}).Warnf(errStr+"%s", err)
								time.Sleep(time.Second * waitSeconds)
								continue
							}
							//5.try  declare queue  on channel
							_, _, err = queueDeclare(getConsumerKey(_item.message, _item.consumer),
								_item.message.Durable)
							if err != nil {
								pools.Put(conn)
								ctx1.With(logger.Fields{"call": "queueDeclare"}).Warnf("fail,", key, err)
								time.Sleep(time.Second * waitSeconds)
								continue
							}
							//6.try  bind queue to exchange
							err = queueBindToExchange(getConsumerKey(_item.message, _item.consumer),
								_item.message.Name,
								_item.consumer.RouteKey)
							if err != nil {
								pools.Put(conn)
								ctx1.With(logger.Fields{"call": "queueBindToExchange"}).Warnf(errStr+"%s", err)
								time.Sleep(time.Second * waitSeconds)
								continue
							}
							//7.try  set qos on channel
							err = channel.Qos(1, 0, false)
							if err != nil {
								pools.Put(conn)
								ctx1.With(logger.Fields{"call": "channel.Qos"}).Warnf(errStr+"%s", err)
								time.Sleep(time.Second * waitSeconds)
								continue
							}
							//8.try consume queue
							deliveryChn, err := channel.Consume(getQueueName(key), "", false, false, false, false, nil)
							if err != nil {
								pools.Put(conn)
								ctx1.With(logger.Fields{"call": "channel.Consume"}).Warnf(errStr+"%s", err)
								time.Sleep(time.Second * waitSeconds)
								continue
							}
							ctx1.Infof("waiting for message ...")
							//worker loop,use chan waiting for control command or delivery
							for {
								if _, ok := wrapedConsumers.Get(key); !ok {
									ctx1.Warnf("not found , now exit")
									runtime.Goexit()
								}
								//update consumer active time
								item, _ := wrapedConsumers.Get(key)
								_item := item.(manageConsumer)
								_item.lasttime = time.Now().Unix()
								wrapedConsumers.Set(key, _item)
								select {
								case cmd := <-_item.consumerReadChan:
									if cmd == "exit" {
										pools.Put(conn)
										_item.consumerWriteChan <- "exit_ok"
										runtime.Goexit()
									}
								case delivery, ok := <-deliveryChn:
									if !ok {
										pools.Put(conn)
										ctx1.Warnf("read deliveryChn fail")
										goto RETRY
									}
									//body := string(delivery.Body)[0:50] + "..."
									body := string(delivery.Body)
									ctx1.Debugf("delivery revecived: %s,%s", key, body)
									if process(string(delivery.Body), _item.consumer) == nil {
										//process success
										err = delivery.Ack(false)
										if err != nil {
											ctx1.Warnf("ack fail , %s", err)
											time.Sleep(time.Second * waitSeconds)
										}
									} else {
										//process fail
										err = delivery.Nack(false, true)
										if err != nil {
											ctx1.Warnf("nack fail , %s", err)
											time.Sleep(time.Second * waitSeconds)
										} else {
											time.Sleep(time.Second * waitSeconds1)
										}
									}
								}
							}
						}
					}()
				}
				consumerManageWriteChan <- "completed"
			case "delete": //stop consumer
				if item, ok := wrapedConsumers.Get(key); ok {
					_item := item.(manageConsumer)
					ctx1.Debugf("sending stop singal to goroutine ...")
					_item.consumerReadChan <- "exit"
					ctx1.Debugf("send  stop singal to goroutine success")
				}
				consumerManageWriteChan <- "completed"
			case "status": //get consumer last active time
				v, ok := wrapedConsumers.Get(key)
				var t int64
				if ok {
					_v := v.(manageConsumer)
					t = _v.lasttime
				}
				consumerManageWriteChan <- fmt.Sprintf("%d", t)
			default:
				consumerManageWriteChan <- "unkown command"
			}
		}
	}()
}
func writeMessagesToFile(messages0 []message, configFilePath0 string) (err error) {
	msgLock.Lock()
	defer msgLock.Unlock()
	b, err := json.Marshal(messages0)
	c, err := gabs.ParseJSON(b)
	content := c.StringIndent("", "	")
	err = ioutil.WriteFile(configFilePath0, []byte(content), 0600)
	return
}
func loadMessagesFromFile(configFilePath0 string) (messages0 []message, err error) {
	msgLock.Lock()
	defer msgLock.Unlock()
	if _, err := os.Stat(configFilePath0); os.IsNotExist(err) {
		err = ioutil.WriteFile(configFilePath0, []byte("[]"), 0600)
	} else {
		var content string
		content, err = fileGetContents(configFilePath0)
		if err == nil {
			messages0, err = parseMessages(content)
			if err == nil {
				messages = messages0
			}
		}
	}
	return
}

func process(content string, c consumer) (err error) {
	ctx := ctxFunc("process")
	//content = "{\"body\":\"sss\",\"header\":{\"ID\":\"test\"},\"ip\":\"127.0.0.1\",\"method\":\"get\"}"

	jsonParsed, err := gabs.ParseJSON([]byte(content))
	ctx1 := ctx.With(logger.Fields{"call": "gabs.ParseJSON"})
	if err != nil {
		ctx1.Warnf("message from rabbitmq not suppported and drop it, msg : %s", content[:20])
		return nil
	}
	if !jsonParsed.Exists("body") || !jsonParsed.Exists("header") ||
		!jsonParsed.Exists("ip") || !jsonParsed.Exists("method") || !jsonParsed.Exists("args") {
		ctx1.Warnf("message from rabbitmq not suppported and drop it, msg : %s", content[:20])
		return nil
	}
	body := jsonParsed.S("body").Data().(string)
	header, _ := jsonParsed.S("header").ChildrenMap()
	ip := jsonParsed.S("ip").Data().(string)
	args := jsonParsed.S("args").Data().(string)
	method := jsonParsed.S("method").Data().(string)
	url := c.URL
	if args != "" {
		if strings.Contains(c.URL, "?") {
			url = c.URL + "&" + args
		} else {
			url = c.URL + "?" + args
		}
	}
	var headerMap = make(map[string]string)
	for k, child := range header {
		headerMap[k] = child.Data().(string)
	}
	client := fasthttp.Client{}
	client.MaxConnsPerHost = 65535
	req := &fasthttp.Request{}
	resp := &fasthttp.Response{}
	req.SetRequestURI(url)
	for k, v := range headerMap {
		req.Header.Set(k, v)
	}
	req.Header.Set(cfg.GetString("publish.RealIpHeader"), ip)
	req.Header.SetUserAgent("wmq v" + cfg.GetString("wmq.version") + " - https://github.com/snail007/wmq")

	ctx2 := ctx.With(logger.Fields{"http": c.URL, "method": method})
	if method == "post" {
		if body != "" {
			var decodeBytes []byte
			decodeBytes, err = base64.StdEncoding.DecodeString(body)
			if err != nil {
				err = nil
				ctx2.Warnf("decode post body fail and drop it , content : " + content)
				return
			}
			req.SetBody(decodeBytes)
		}
		req.Header.SetMethod("POST")
	} else if method == "get" {
		req.Header.SetMethod("GET")
	} else {
		err = fmt.Errorf("method [ %s ] not supported", method)
		ctx2.Warnf("consume fail,%s", err)
		return
	}
	//log.Warnf("%s", req)
	err = client.DoTimeout(req, resp, time.Duration(time.Duration(c.Timeout)*time.Millisecond))
	if err != nil {
		ctx2.Warnf("consume fail,%s", err)
		return
	}
	if c.CheckCode {
		code := resp.StatusCode()
		ctx3 := ctx2.With(logger.Fields{"httpCode": strconv.Itoa(code)})
		if float64(code) != c.Code {
			err = fmt.Errorf("consume fail,httpCode 200 expected ")
			ctx3.Warnf("%s", err)
		} else {
			ctx3.Debugf("consume success")
		}
	} else {
		ctx2.Debugf("consume success")
	}
	return
}
