// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

package webrtc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/logging"
	"github.com/pion/transport/v3/test"
	"github.com/stretchr/testify/assert"
)

func TestDataChannel_EventHandlers(t *testing.T) {
	to := test.TimeOut(time.Second * 20)
	defer to.Stop()

	report := test.CheckRoutines(t)
	defer report()

	api := NewAPI()
	dc := &DataChannel{api: api}

	onDialCalled := make(chan struct{})
	onOpenCalled := make(chan struct{})
	onMessageCalled := make(chan struct{})

	// Verify that the noop case works
	assert.NotPanics(t, func() { dc.onOpen() })

	dc.OnDial(func() {
		close(onDialCalled)
	})

	dc.OnOpen(func() {
		close(onOpenCalled)
	})

	dc.OnMessage(func(DataChannelMessage) {
		close(onMessageCalled)
	})

	// Verify that the set handlers are called
	assert.NotPanics(t, func() { dc.onDial() })
	assert.NotPanics(t, func() { dc.onOpen() })
	assert.NotPanics(t, func() { dc.onMessage(DataChannelMessage{Data: []byte("o hai")}) })

	// Wait for all handlers to be called
	<-onDialCalled
	<-onOpenCalled
	<-onMessageCalled
}

func TestDataChannel_MessagesAreOrdered(t *testing.T) {
	report := test.CheckRoutines(t)
	defer report()

	api := NewAPI()
	dc := &DataChannel{api: api}

	maxVal := 512
	out := make(chan int)
	inner := func(msg DataChannelMessage) {
		// randomly sleep
		// math/rand a weak RNG, but this does not need to be secure. Ignore with #nosec
		/* #nosec */
		randInt, err := rand.Int(rand.Reader, big.NewInt(int64(maxVal)))
		assert.NoError(t, err, "Failed to get random sleep duration")
		time.Sleep(time.Duration(randInt.Int64()) * time.Microsecond)
		s, _ := binary.Varint(msg.Data)
		out <- int(s)
	}
	dc.OnMessage(func(p DataChannelMessage) {
		inner(p)
	})

	go func() {
		for i := 1; i <= maxVal; i++ {
			buf := make([]byte, 8)
			binary.PutVarint(buf, int64(i))
			dc.onMessage(DataChannelMessage{Data: buf})
			// Change the registered handler a couple of times to make sure
			// that everything continues to work, we don't lose messages, etc.
			if i%2 == 0 {
				handler := func(msg DataChannelMessage) {
					inner(msg)
				}
				dc.OnMessage(handler)
			}
		}
	}()

	values := make([]int, 0, maxVal)
	for v := range out {
		values = append(values, v)
		if len(values) == maxVal {
			close(out)
		}
	}

	expected := make([]int, maxVal)
	for i := 1; i <= maxVal; i++ {
		expected[i-1] = i
	}
	assert.EqualValues(t, expected, values)
}

// Note(albrow): This test includes some features that aren't supported by the
// Wasm bindings (at least for now).
func TestDataChannelParamters_Go(t *testing.T) {
	report := test.CheckRoutines(t)
	defer report()

	t.Run("MaxPacketLifeTime exchange", func(t *testing.T) {
		ordered := true
		var maxPacketLifeTime uint16 = 3
		options := &DataChannelInit{
			Ordered:           &ordered,
			MaxPacketLifeTime: &maxPacketLifeTime,
		}

		offerPC, answerPC, dc, done := setUpDataChannelParametersTest(t, options)

		// Check if parameters are correctly set
		assert.True(t, dc.Ordered(), "Ordered should be set to true")
		if assert.NotNil(t, dc.MaxPacketLifeTime(), "should not be nil") {
			assert.Equal(t, maxPacketLifeTime, *dc.MaxPacketLifeTime(), "should match")
		}

		answerPC.OnDataChannel(func(d *DataChannel) {
			// Make sure this is the data channel we were looking for. (Not the one
			// created in signalPair).
			if d.Label() != expectedLabel {
				return
			}

			// Check if parameters are correctly set
			assert.True(t, d.ordered, "Ordered should be set to true")
			if assert.NotNil(t, d.maxPacketLifeTime, "should not be nil") {
				assert.Equal(t, maxPacketLifeTime, *d.maxPacketLifeTime, "should match")
			}
			done <- true
		})

		closeReliabilityParamTest(t, offerPC, answerPC, done)
	})

	t.Run("All other property methods", func(t *testing.T) {
		id := uint16(123)
		dc := &DataChannel{}
		dc.id = &id
		dc.label = "mylabel"
		dc.protocol = "myprotocol"
		dc.negotiated = true

		assert.Equal(t, dc.id, dc.ID(), "should match")
		assert.Equal(t, dc.label, dc.Label(), "should match")
		assert.Equal(t, dc.protocol, dc.Protocol(), "should match")
		assert.Equal(t, dc.negotiated, dc.Negotiated(), "should match")
		assert.Equal(t, uint64(0), dc.BufferedAmount(), "should match")
		dc.SetBufferedAmountLowThreshold(1500)
		assert.Equal(t, uint64(1500), dc.BufferedAmountLowThreshold(), "should match")
	})
}

func TestDataChannelBufferedAmount(t *testing.T) { //nolint:cyclop
	t.Run("set before datachannel becomes open", func(t *testing.T) {
		report := test.CheckRoutines(t)
		defer report()

		var nOfferBufferedAmountLowCbs uint32
		var offerBufferedAmountLowThreshold uint64 = 1500
		var nAnswerBufferedAmountLowCbs uint32
		var answerBufferedAmountLowThreshold uint64 = 1400

		buf := make([]byte, 1000)

		offerPC, answerPC, err := newPair()
		assert.NoError(t, err)

		nPacketsToSend := int(10)
		var nOfferReceived uint32
		var nAnswerReceived uint32

		done := make(chan bool)

		answerPC.OnDataChannel(func(answerDC *DataChannel) {
			// Make sure this is the data channel we were looking for. (Not the one
			// created in signalPair).
			if answerDC.Label() != expectedLabel {
				return
			}

			answerDC.OnOpen(func() {
				assert.Equal(t, answerBufferedAmountLowThreshold, answerDC.BufferedAmountLowThreshold(), "value mismatch")

				for i := 0; i < nPacketsToSend; i++ {
					e := answerDC.Send(buf)
					assert.NoError(t, e, "Failed to send string on data channel")
				}
			})

			answerDC.OnMessage(func(DataChannelMessage) {
				atomic.AddUint32(&nAnswerReceived, 1)
			})
			assert.True(t, answerDC.Ordered(), "Ordered should be set to true")

			// The value is temporarily stored in the answerDC object
			// until the answerDC gets opened
			answerDC.SetBufferedAmountLowThreshold(answerBufferedAmountLowThreshold)
			// The callback function is temporarily stored in the answerDC object
			// until the answerDC gets opened
			answerDC.OnBufferedAmountLow(func() {
				atomic.AddUint32(&nAnswerBufferedAmountLowCbs, 1)
				if atomic.LoadUint32(&nOfferBufferedAmountLowCbs) > 0 {
					done <- true
				}
			})
		})

		offerDC, err := offerPC.CreateDataChannel(expectedLabel, nil)
		assert.NoError(t, err, "Failed to create a PC pair for testing")
		assert.True(t, offerDC.Ordered(), "Ordered should be set to true")

		offerDC.OnOpen(func() {
			assert.Equal(t, offerBufferedAmountLowThreshold, offerDC.BufferedAmountLowThreshold(), "value mismatch")

			for i := 0; i < nPacketsToSend; i++ {
				e := offerDC.Send(buf)
				assert.NoError(t, e, "Failed to send string on data channel")
				// assert.Equal(t, (i+1)*len(buf), int(offerDC.BufferedAmount()), "unexpected bufferedAmount")
			}
		})

		offerDC.OnMessage(func(DataChannelMessage) {
			atomic.AddUint32(&nOfferReceived, 1)
		})

		// The value is temporarily stored in the offerDC object
		// until the offerDC gets opened
		offerDC.SetBufferedAmountLowThreshold(offerBufferedAmountLowThreshold)
		// The callback function is temporarily stored in the offerDC object
		// until the offerDC gets opened
		offerDC.OnBufferedAmountLow(func() {
			atomic.AddUint32(&nOfferBufferedAmountLowCbs, 1)
			if atomic.LoadUint32(&nAnswerBufferedAmountLowCbs) > 0 {
				done <- true
			}
		})

		err = signalPair(offerPC, answerPC)
		assert.NoError(t, err, "Failed to signal our PC pair for testing")

		closePair(t, offerPC, answerPC, done)

		t.Logf("nOfferBufferedAmountLowCbs : %d", nOfferBufferedAmountLowCbs)
		t.Logf("nAnswerBufferedAmountLowCbs: %d", nAnswerBufferedAmountLowCbs)
		assert.True(t, nOfferBufferedAmountLowCbs > uint32(0), "callback should be made at least once")
		assert.True(t, nAnswerBufferedAmountLowCbs > uint32(0), "callback should be made at least once")
	})

	t.Run("set after datachannel becomes open", func(t *testing.T) {
		report := test.CheckRoutines(t)
		defer report()

		var nCbs int
		buf := make([]byte, 1000)

		offerPC, answerPC, err := newPair()
		assert.NoError(t, err)

		done := make(chan bool)

		answerPC.OnDataChannel(func(dataChannel *DataChannel) {
			// Make sure this is the data channel we were looking for. (Not the one
			// created in signalPair).
			if dataChannel.Label() != expectedLabel {
				return
			}
			var nPacketsReceived int
			dataChannel.OnMessage(func(DataChannelMessage) {
				nPacketsReceived++

				if nPacketsReceived == 10 {
					go func() {
						time.Sleep(time.Second)
						done <- true
					}()
				}
			})
			assert.True(t, dataChannel.Ordered(), "Ordered should be set to true")
		})

		dc, err := offerPC.CreateDataChannel(expectedLabel, nil)
		assert.NoError(t, err)

		assert.True(t, dc.Ordered(), "Ordered should be set to true")

		dc.OnOpen(func() {
			// The value should directly be passed to sctp
			dc.SetBufferedAmountLowThreshold(1500)
			// The callback function should directly be passed to sctp
			dc.OnBufferedAmountLow(func() {
				nCbs++
			})

			for i := 0; i < 10; i++ {
				assert.NoError(t, dc.Send(buf), "Failed to send string on data channel")
				assert.Equal(t, uint64(1500), dc.BufferedAmountLowThreshold(), "value mismatch")
				// assert.Equal(t, (i+1)*len(buf), int(dc.BufferedAmount()), "unexpected bufferedAmount")
			}
		})

		dc.OnMessage(func(DataChannelMessage) {
		})

		assert.NoError(t, signalPair(offerPC, answerPC))

		closePair(t, offerPC, answerPC, done)

		assert.True(t, nCbs > 0, "callback should be made at least once")
	})
}

func TestEOF(t *testing.T) { //nolint:cyclop
	t.Helper()

	report := test.CheckRoutines(t)
	defer report()

	log := logging.NewDefaultLoggerFactory().NewLogger("test")
	label := "test-channel"
	testData := []byte("this is some test data")

	t.Run("Detach", func(t *testing.T) {
		// Use Detach data channels mode
		s := SettingEngine{}
		s.DetachDataChannels()
		api := NewAPI(WithSettingEngine(s))

		// Set up two peer connections.
		config := Configuration{}
		pca, err := api.NewPeerConnection(config)
		assert.NoError(t, err)
		pcb, err := api.NewPeerConnection(config)
		assert.NoError(t, err)

		defer closePairNow(t, pca, pcb)

		var wg sync.WaitGroup

		dcChan := make(chan datachannel.ReadWriteCloser)
		pcb.OnDataChannel(func(dc *DataChannel) {
			if dc.Label() != label {
				return
			}
			log.Debug("OnDataChannel was called")
			dc.OnOpen(func() {
				detached, err2 := dc.Detach()
				assert.NoError(t, err2, "Detach failed")

				dcChan <- detached
			})
		})

		wg.Add(1)
		go func() {
			defer wg.Done()

			var msg []byte

			log.Debug("Waiting for OnDataChannel")
			dc := <-dcChan
			log.Debug("data channel opened")
			defer func() { assert.NoError(t, dc.Close(), "should succeed") }()

			log.Debug("Waiting for ping...")
			msg, err2 := io.ReadAll(dc)
			log.Debugf("Received ping! \"%s\"", string(msg))
			assert.NoError(t, err2)

			assert.Equal(t, testData, msg)
		}()

		assert.NoError(t, signalPair(pca, pcb))

		attached, err := pca.CreateDataChannel(label, nil)
		assert.NoError(t, err)
		log.Debug("Waiting for data channel to open")
		open := make(chan struct{})
		attached.OnOpen(func() {
			open <- struct{}{}
		})
		<-open
		log.Debug("data channel opened")

		var dc io.ReadWriteCloser
		dc, err = attached.Detach()
		assert.NoError(t, err)

		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Debug("Sending ping...")
			_, err = dc.Write(testData)
			assert.NoError(t, err)
			log.Debug("Sent ping")

			assert.NoError(t, dc.Close(), "should succeed")

			log.Debug("Wating for EOF")
			ret, err2 := io.ReadAll(dc)
			assert.Nil(t, err2, "should succeed")
			assert.Equal(t, 0, len(ret), "should be empty")
		}()

		wg.Wait()
	})

	t.Run("No detach", func(t *testing.T) {
		lim := test.TimeOut(time.Second * 5)
		defer lim.Stop()

		// Set up two peer connections.
		config := Configuration{}
		pca, err := NewPeerConnection(config)
		assert.NoError(t, err)
		pcb, err := NewPeerConnection(config)
		assert.NoError(t, err)

		defer closePairNow(t, pca, pcb)

		var dca, dcb *DataChannel
		dcaClosedCh := make(chan struct{})
		dcbClosedCh := make(chan struct{})

		pcb.OnDataChannel(func(dc *DataChannel) {
			if dc.Label() != label {
				return
			}

			log.Debugf("pcb: new datachannel: %s", dc.Label())

			dcb = dc
			// Register channel opening handling
			dcb.OnOpen(func() {
				log.Debug("pcb: datachannel opened")
			})

			dcb.OnClose(func() {
				// (2)
				log.Debug("pcb: data channel closed")
				close(dcbClosedCh)
			})

			// Register the OnMessage to handle incoming messages
			log.Debug("pcb: registering onMessage callback")
			dcb.OnMessage(func(dcMsg DataChannelMessage) {
				log.Debugf("pcb: received ping: %s", string(dcMsg.Data))
				assert.Equal(t, testData, dcMsg.Data)
			})
		})

		dca, err = pca.CreateDataChannel(label, nil)
		assert.NoError(t, err)

		dca.OnOpen(func() {
			log.Debug("pca: data channel opened")
			log.Debugf("pca: sending \"%s\"", string(testData))
			assert.NoError(t, dca.Send(testData))
			log.Debug("pca: sent ping")
			assert.NoError(t, dca.Close(), "should succeed") // <-- dca closes
		})

		dca.OnClose(func() {
			// (1)
			log.Debug("pca: data channel closed")
			close(dcaClosedCh)
		})

		// Register the OnMessage to handle incoming messages
		log.Debug("pca: registering onMessage callback")
		dca.OnMessage(func(dcMsg DataChannelMessage) {
			log.Debugf("pca: received pong: %s", string(dcMsg.Data))
			assert.Equal(t, testData, dcMsg.Data)
		})

		assert.NoError(t, signalPair(pca, pcb))

		// When dca closes the channel,
		// (1) dca.Onclose() will fire immediately, then
		// (2) dcb.OnClose will also fire
		<-dcaClosedCh // (1)
		<-dcbClosedCh // (2)
	})
}

// Assert that a Session Description that doesn't follow
// draft-ietf-mmusic-sctp-sdp is still accepted.
func TestDataChannel_NonStandardSessionDescription(t *testing.T) {
	to := test.TimeOut(time.Second * 20)
	defer to.Stop()

	report := test.CheckRoutines(t)
	defer report()

	offerPC, answerPC, err := newPair()
	assert.NoError(t, err)

	_, err = offerPC.CreateDataChannel("foo", nil)
	assert.NoError(t, err)

	onDataChannelCalled := make(chan struct{})
	answerPC.OnDataChannel(func(_ *DataChannel) {
		close(onDataChannelCalled)
	})

	offer, err := offerPC.CreateOffer(nil)
	assert.NoError(t, err)

	offerGatheringComplete := GatheringCompletePromise(offerPC)
	assert.NoError(t, offerPC.SetLocalDescription(offer))
	<-offerGatheringComplete

	offer = *offerPC.LocalDescription()

	// Replace with old values
	const (
		oldApplication = "m=application 63743 DTLS/SCTP 5000\r"
		oldAttribute   = "a=sctpmap:5000 webrtc-datachannel 256\r"
	)

	offer.SDP = regexp.MustCompile(`m=application (.*?)\r`).ReplaceAllString(offer.SDP, oldApplication)
	offer.SDP = regexp.MustCompile(`a=sctp-port(.*?)\r`).ReplaceAllString(offer.SDP, oldAttribute)

	// Assert that replace worked
	assert.True(t, strings.Contains(offer.SDP, oldApplication))
	assert.True(t, strings.Contains(offer.SDP, oldAttribute))

	assert.NoError(t, answerPC.SetRemoteDescription(offer))

	answer, err := answerPC.CreateAnswer(nil)
	assert.NoError(t, err)

	answerGatheringComplete := GatheringCompletePromise(answerPC)
	assert.NoError(t, answerPC.SetLocalDescription(answer))
	<-answerGatheringComplete
	assert.NoError(t, offerPC.SetRemoteDescription(*answerPC.LocalDescription()))

	<-onDataChannelCalled
	closePairNow(t, offerPC, answerPC)
}

func TestDataChannel_Dial(t *testing.T) {
	t.Run("handler should be called once, by dialing peer only", func(t *testing.T) {
		report := test.CheckRoutines(t)
		defer report()

		dialCalls := make(chan bool, 2)
		wg := new(sync.WaitGroup)
		wg.Add(2)

		offerPC, answerPC, err := newPair()
		assert.NoError(t, err)

		answerPC.OnDataChannel(func(d *DataChannel) {
			if d.Label() != expectedLabel {
				return
			}

			d.OnDial(func() {
				// only dialing side should fire OnDial
				assert.Fail(t, "answering side should not call on dial")
			})

			d.OnOpen(wg.Done)
		})

		d, err := offerPC.CreateDataChannel(expectedLabel, nil)
		assert.NoError(t, err)
		d.OnDial(func() {
			dialCalls <- true
			wg.Done()
		})

		assert.NoError(t, signalPair(offerPC, answerPC))

		wg.Wait()
		closePairNow(t, offerPC, answerPC)

		assert.Len(t, dialCalls, 1)
	})

	t.Run("handler should be called immediately if already dialed", func(t *testing.T) {
		report := test.CheckRoutines(t)
		defer report()

		done := make(chan bool)

		offerPC, answerPC, err := newPair()
		assert.NoError(t, err)

		d, err := offerPC.CreateDataChannel(expectedLabel, nil)
		assert.NoError(t, err)
		d.OnOpen(func() {
			// when the offer DC has been opened, its guaranteed to have dialed since it has
			// received a response to said dial. this test represents an unrealistic usage,
			// but its the best way to guarantee we "missed" the dial event and still invoke
			// the handler.
			d.OnDial(func() {
				done <- true
			})
		})

		assert.NoError(t, signalPair(offerPC, answerPC))

		closePair(t, offerPC, answerPC, done)
	})
}

func TestDetachRemovesDatachannelReference(t *testing.T) {
	// Use Detach data channels mode
	s := SettingEngine{}
	s.DetachDataChannels()
	api := NewAPI(WithSettingEngine(s))

	// Set up two peer connections.
	config := Configuration{}
	pca, err := api.NewPeerConnection(config)
	assert.NoError(t, err)
	pcb, err := api.NewPeerConnection(config)
	assert.NoError(t, err)

	defer closePairNow(t, pca, pcb)

	dcChan := make(chan *DataChannel, 1)
	pcb.OnDataChannel(func(d *DataChannel) {
		d.OnOpen(func() {
			_, detachErr := d.Detach()
			assert.NoError(t, detachErr)

			dcChan <- d
		})
	})

	assert.NoError(t, signalPair(pca, pcb))

	attached, err := pca.CreateDataChannel("", nil)
	assert.NoError(t, err)
	open := make(chan struct{}, 1)
	attached.OnOpen(func() {
		open <- struct{}{}
	})
	<-open

	d := <-dcChan
	d.sctpTransport.lock.RLock()
	defer d.sctpTransport.lock.RUnlock()
	for _, dc := range d.sctpTransport.dataChannels[:cap(d.sctpTransport.dataChannels)] {
		assert.NotEqual(t, dc, d, "expected sctpTransport to drop reference to datachannel")
	}
}

func TestDataChannelClose(t *testing.T) {
	// Test if onClose is fired for self and remote after Close is called
	t.Run("close open channels", func(t *testing.T) {
		options := &DataChannelInit{}

		offerPC, answerPC, dc, done := setUpDataChannelParametersTest(t, options)

		answerPC.OnDataChannel(func(dataChannel *DataChannel) {
			// Make sure this is the data channel we were looking for. (Not the one
			// created in signalPair).
			if dataChannel.Label() != expectedLabel {
				return
			}

			dataChannel.OnOpen(func() {
				assert.NoError(t, dataChannel.Close())
			})

			dataChannel.OnClose(func() {
				done <- true
			})
		})

		dc.OnClose(func() {
			done <- true
		})

		assert.NoError(t, signalPair(offerPC, answerPC))

		// Offer and Answer OnClose
		<-done
		<-done

		assert.NoError(t, offerPC.Close())
		assert.NoError(t, answerPC.Close())
	})

	// Test if OnClose is fired for self and remote after Close is called on non-established channel
	// https://github.com/pion/webrtc/issues/2659
	t.Run("Close connecting channels", func(t *testing.T) {
		options := &DataChannelInit{}

		offerPC, answerPC, dc, done := setUpDataChannelParametersTest(t, options)

		answerPC.OnDataChannel(func(dataChannel *DataChannel) {
			// Make sure this is the data channel we were looking for. (Not the one
			// created in signalPair).
			if dataChannel.Label() != expectedLabel {
				return
			}

			dataChannel.OnOpen(func() {
				assert.Fail(t, "OnOpen must not be fired after we call Close")
			})

			dataChannel.OnClose(func() {
				done <- true
			})

			assert.NoError(t, dataChannel.Close())
		})

		dc.OnClose(func() {
			done <- true
		})

		assert.NoError(t, signalPair(offerPC, answerPC))

		// Offer and Answer OnClose
		<-done
		<-done

		assert.NoError(t, offerPC.Close())
		assert.NoError(t, answerPC.Close())
	})
}

func TestDataChannel_DetachErrors(t *testing.T) {
	t.Run("error errDetachNotEnabled", func(t *testing.T) {
		s := SettingEngine{}
		offer, answer, err := NewAPI(WithSettingEngine(s)).newPair(Configuration{})
		assert.NoError(t, err)
		dc, err := offer.CreateDataChannel("data", nil)
		assert.NoError(t, err)
		_, err = dc.Detach()
		assert.ErrorIs(t, err, errDetachNotEnabled)
		assert.NoError(t, offer.Close())
		assert.NoError(t, answer.Close())
	})

	t.Run("error errDetachBeforeOpened", func(t *testing.T) {
		s := SettingEngine{}
		s.DetachDataChannels()
		offer, answer, err := NewAPI(WithSettingEngine(s)).newPair(Configuration{})
		assert.NoError(t, err)
		dc, err := offer.CreateDataChannel("data", nil)
		assert.NoError(t, err)
		_, err = dc.Detach()
		assert.ErrorIs(t, err, errDetachBeforeOpened)
		assert.NoError(t, offer.Close())
		assert.NoError(t, answer.Close())
	})
}

func TestDataChannelMessageSize(t *testing.T) {
	offerPC, answerPC, err := newPair()
	assert.NoError(t, err)

	dc, err := offerPC.CreateDataChannel("", nil)
	assert.NoError(t, err)

	answerDataChannelMessages := make(chan []byte)
	answerPC.OnDataChannel(func(d *DataChannel) {
		d.OnMessage(func(m DataChannelMessage) {
			answerDataChannelMessages <- m.Data
		})
	})

	assert.NoError(t, signalPair(offerPC, answerPC))

	messagesSent, messagesSentCancel := context.WithCancel(context.Background())
	dc.OnOpen(func() {
		for i := 0; i <= 10; i++ {
			outboundMessage := make([]byte, sctpMaxMessageSizeUnsetValue*i)
			_, err := rand.Read(outboundMessage)
			assert.NoError(t, err)

			assert.NoError(t, dc.Send(outboundMessage))
			inboundMessage := <-answerDataChannelMessages

			assert.Equal(t, outboundMessage, inboundMessage)
		}
		messagesSentCancel()
	})

	<-messagesSent.Done()
	closePairNow(t, offerPC, answerPC)
}
