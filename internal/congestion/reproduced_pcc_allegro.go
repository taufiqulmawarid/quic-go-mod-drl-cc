package congestion

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"time"

	deque "github.com/edwingeng/deque/v2"
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/quic-go/quic-go/logging"
)

var PccDefaultNRttInterval float64 = 3

const (
	PccStartingSendingRate             Bandwidth = 512 * 1000
	PccKProbingStepSize                          = 0.01
	PccKMaxProbingStepSize                       = 0.05
	PccKDecisionMadeStepSize                     = 0.02
	PccKInitialRTT                               = 10 * time.Millisecond
	PccKNumIntervalGroupsInProbing               = 2
	PccKRttFluctuationToleranceRatio             = 0.2
	PccKConstantInitialMaxDatagramSize           = protocol.ByteCount(protocol.InitialPacketSizeIPv4)
)

// Type IntervalRTTEstimator
var IntervalRTTEstimatorType int = IntervalRTTSmoothedEstimatorType

const (
	IntervalRTTSmoothedEstimatorType      = iota // 0
	IntervalRTTJacobsonKarelEstimatorType        // 1
	IntervalRTTMinFilterEstimatorType            // 2
)

type PccSendMode uint8

const (
	PccSenderModeStarting PccSendMode = iota
	PccSenderModePccProbing
	PccSenderModePccDecisionMade
)

type PccRateDirection uint8

const (
	PccRateDirectionIncrease PccRateDirection = iota
	PccRateDirectionDecrease
)

type ReproducedPccAllegroSender struct {
	rttStats *utils.RTTStats
	pacer    *pacer

	// Control system
	Mode PccSendMode

	// Sending rate in bits per-second.
	sendingRate Bandwidth

	// Set later after handshake is confirmed.
	maxDatagramSize protocol.ByteCount

	// Timer variables to manage the interval
	lastTimeChangeSendingRate time.Time

	// PCC Central Sending Rate
	pccCentralSendingRate Bandwidth
	// Rate Direction
	pccRateDirection PccRateDirection
	// Try to collect PCC Monitor Interval
	pccIntervalRTTEstimator     IntervalRTTEstimator
	pccNRttInterval             float64
	pccMonitorIntervals         *deque.Deque[*PccMonitorInterval]
	pccMonitorIntervalNumUseful int
	// Number of rounds sender remains in current mode.
	pccRounds int

	// Tracing and logging
	lastState logging.CongestionState
	tracer    logging.ConnectionTracer
}

const (
	// Tolerance of loss rate by utility function.
	pccKLossTolerance = 0.05
	// Coefficeint of the loss rate term in utility function.
	pccKLossCoefficient = -1000000.0
	// Coefficient of RTT term in utility function.
	// pccKRTTCoefficient = -200.0
)

type PccMonitorInterval struct {
	SendingRate Bandwidth
	// True if calculating utility for this MonitorInterval.
	IsUseful                     bool
	RttFluctuationToleranceRatio float64
	FirstPacketSentTime          time.Time
	LastPacketSentTime           time.Time
	FirstPacketNumber            protocol.PacketNumber
	LastPacketNumber             protocol.PacketNumber
	BytesSent                    protocol.ByteCount
	BytesAcked                   protocol.ByteCount
	BytesLost                    protocol.ByteCount
	RttOnMonitorStart            time.Duration
	RttOnMonitorEnd              time.Duration
	// Added new var to get better tput sampling
	FirstPacketAckedTime time.Time
	LastPacketAckedTime  time.Time
}

func NewReproducedPccAllegroSender(
	rttStats *utils.RTTStats,
	initialMaxDatagramSize protocol.ByteCount,
	tracer logging.ConnectionTracer,
) *ReproducedPccAllegroSender {
	return newReproducedPccAllegroSender(
		rttStats,
		initialMaxDatagramSize,
		initialCongestionWindow*initialMaxDatagramSize,
		protocol.MaxCongestionWindowPackets*initialMaxDatagramSize,
		tracer,
	)
}

func newReproducedPccAllegroSender(
	rttStats *utils.RTTStats,
	initialMaxDatagramSize,
	initialCongestionWindow,
	initialMaxCongestionWindow protocol.ByteCount,
	tracer logging.ConnectionTracer,
) *ReproducedPccAllegroSender {
	// Collect command's arguments
	for i, ivar := range os.Args {
		if ivar == "-interval-rtt-estimator" {
			argVal := os.Args[i+1]
			intVal, err := strconv.Atoi(argVal)
			if err != nil {
				panic("The data type of interval-rtt-estimator is not int")
			}
			IntervalRTTEstimatorType = intVal
		} else if ivar == "-interval-rtt-n" {
			argVal := os.Args[i+1]
			floatVal, err := strconv.ParseFloat(argVal, 64)
			if err != nil {
				panic("The data type of interval-rtt-n is not float")
			}
			PccDefaultNRttInterval = floatVal
		}
	}

	c := &ReproducedPccAllegroSender{
		rttStats:        rttStats,
		tracer:          tracer,
		maxDatagramSize: initialMaxDatagramSize,
	}

	// Configuring Starting Mode
	c.Mode = PccSenderModeStarting
	c.pccCentralSendingRate = PccStartingSendingRate
	c.sendingRate = c.pccCentralSendingRate * 1
	c.pccRateDirection = PccRateDirectionIncrease
	c.lastTimeChangeSendingRate = time.Now()
	c.pccRounds = 1

	// Create initial PCC Monitor Interval
	c.pccNRttInterval = PccDefaultNRttInterval
	c.pccIntervalRTTEstimator = NewIntervalRTTEstimator(IntervalRTTEstimatorType, rttStats)
	c.pccMonitorIntervals = deque.NewDeque[*PccMonitorInterval]()
	c.pccMonitorIntervals.PushBack(
		&PccMonitorInterval{
			SendingRate:                  c.sendingRate,
			IsUseful:                     false,
			RttFluctuationToleranceRatio: PccKRttFluctuationToleranceRatio,
			RttOnMonitorStart:            PccKInitialRTT,
		},
	)

	c.pacer = newPacer(c.BandwidthEstimate)
	if c.tracer != nil {
		c.lastState = logging.CongestionStateSlowStart
		c.tracer.UpdatedCongestionState(logging.CongestionStateSlowStart)
	}
	return c
}

// TimeUntilSend returns when the next packet should be sent.
func (c *ReproducedPccAllegroSender) TimeUntilSend(_ protocol.ByteCount) time.Time {
	return c.pacer.TimeUntilSend()
}

func (c *ReproducedPccAllegroSender) HasPacingBudget() bool {
	return c.pacer.Budget(time.Now()) >= c.maxDatagramSize
}

func (c *ReproducedPccAllegroSender) OnPacketSent(
	sentTime time.Time,
	bytesInFlight protocol.ByteCount,
	packetNumber protocol.PacketNumber,
	bytes protocol.ByteCount,
	isRetransmittable bool,
) {
	c.pacer.SentPacket(sentTime, bytes)
	if !isRetransmittable {
		return
	}
	isEndOfInterval := c.isEndOfInterval(c.pccNRttInterval)
	isMiEmpty := c.pccMonitorIntervals.IsEmpty()
	// Make sure that the PCC Monitor Interval is not empty
	if isMiEmpty {
		panic("The PCC Monitor Interval is Empty")
	}
	// Update the monitor interval when:
	// 1. Not at the end of interval
	// 2. Monitor interval is not empty
	if !isEndOfInterval && !isMiEmpty {
		c.OnSentPccMonitorInterval(sentTime, packetNumber, bytes)
		return
	}
	switch c.Mode {
	case PccSenderModeStarting:
		if c.pccMonitorIntervalNumUseful < 2 {
			// Buat MI Baru
			newSendingRate := Bandwidth(.5 * float64(c.sendingRate))
			if c.pccMonitorIntervalNumUseful == 1 {
				newSendingRate = Bandwidth(2. * float64(c.sendingRate))
			}
			c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
		} else {
			// Calculate Utility Value
			mis := c.pccMonitorIntervals.Dump()
			misLen := len(mis)
			if misLen < 2 {
				panic("monitor interval is less than 2")
			}
			PastUtilityValue := CalculateUtilityAllegroV1(mis[misLen-2])
			CurrentUtilityValue := CalculateUtilityAllegroV1(mis[misLen-1])
			if CurrentUtilityValue >= PastUtilityValue {
				for i := 0; i < misLen-1; i++ {
					_ = c.pccMonitorIntervals.PopFront()
				}
				newSendingRate := Bandwidth(2. * float64(c.sendingRate))
				c.pccRounds += 1
				c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			} else {
				c.Mode = PccSenderModePccProbing
				c.pccRounds = 1
				// Empty the monitor intervals
				for i := 0; i < misLen; i++ {
					_ = c.pccMonitorIntervals.PopFront()
				}
				c.pccMonitorIntervalNumUseful = 0
				c.pccCentralSendingRate = mis[misLen-2].SendingRate
				// Buat MI baru untuk Probing
				newSendingRate := c.pccCentralSendingRate
				stepSize := float64(c.pccRounds) * PccKProbingStepSize
				if rand.Intn(2) == 0 {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
				} else {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
				}
				c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			}
		}
	case PccSenderModePccProbing:
		if c.pccMonitorIntervalNumUseful < PccKNumIntervalGroupsInProbing*2 {
			newSendingRate := c.pccCentralSendingRate
			stepSize := float64(c.pccRounds) * PccKProbingStepSize
			stepSize = math.Min(stepSize, PccKMaxProbingStepSize)
			if c.pccMonitorIntervalNumUseful%2 == 0 {
				// re-random
				if rand.Intn(2) == 0 {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
				} else {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
				}
			} else {
				// add another direction
				if c.sendingRate <= c.pccCentralSendingRate {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
				} else {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
				}
			}
			c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
		} else {
			// Calculate Utility Value
			var lowerWin, higherWin int64
			for {
				mi1, ok1 := c.pccMonitorIntervals.TryPopFront()
				mi2, ok2 := c.pccMonitorIntervals.TryPopFront()
				if !ok1 || !ok2 {
					break
				}
				// Assume that the first mi is the higher rate.
				// If not, swap mi1 and mi2
				if mi1.SendingRate < c.pccCentralSendingRate {
					mi1, mi2 = mi2, mi1
				}
				higherUtilValue := CalculateUtilityAllegroV1(mi1)
				lowerUtilValue := CalculateUtilityAllegroV1(mi2)
				if higherUtilValue > lowerUtilValue {
					higherWin += 1
				} else {
					lowerWin += 1
				}
			}
			// Decide Next Mode
			if higherWin > lowerWin {
				c.Mode = PccSenderModePccDecisionMade
				c.pccRateDirection = PccRateDirectionIncrease
			} else if higherWin < lowerWin {
				c.Mode = PccSenderModePccDecisionMade
				c.pccRateDirection = PccRateDirectionDecrease
			} else {
				c.Mode = PccSenderModePccProbing
			}
			// Prepare next mi
			c.pccMonitorIntervalNumUseful = 0
			if c.Mode == PccSenderModePccProbing {
				c.pccRounds += 1
				// Homework, 13 Juli 2022, sementara sama dulu
				c.pccCentralSendingRate *= 1
				// Buat MI baru untuk Probing
				newSendingRate := c.pccCentralSendingRate * 1
				stepSize := float64(c.pccRounds) * PccKProbingStepSize
				stepSize = math.Min(stepSize, PccKMaxProbingStepSize)
				if rand.Intn(2) == 0 {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
				} else {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
				}
				c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			} else { // Decision Made
				c.pccRounds = 1
				stepSize := float64(c.pccRounds) * PccKDecisionMadeStepSize
				if c.pccRateDirection == PccRateDirectionIncrease {
					c.pccCentralSendingRate = Bandwidth(float64(c.pccCentralSendingRate) * (1.0 + stepSize))
				} else {
					c.pccCentralSendingRate = Bandwidth(float64(c.pccCentralSendingRate) * (1.0 - stepSize))
				}
				// Buat MI baru untuk Probing
				newSendingRate := c.pccCentralSendingRate
				c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			}
		}
	case PccSenderModePccDecisionMade:
		if c.pccMonitorIntervalNumUseful < 2 {
			// Buat MI Baru
			newSendingRate := c.sendingRate
			stepSize := float64(c.pccRounds) * PccKDecisionMadeStepSize
			if c.pccRateDirection == PccRateDirectionIncrease {
				newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
			} else {
				newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
			}
			c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
		} else {
			// Calculate Utility Value
			mis := c.pccMonitorIntervals.Dump()
			misLen := len(mis)
			if misLen < 2 {
				panic("monitor interval is less than 2")
			}
			PastUtilityValue := CalculateUtilityAllegroV1(mis[misLen-2])
			CurrentUtilityValue := CalculateUtilityAllegroV1(mis[misLen-1])
			if CurrentUtilityValue >= PastUtilityValue {
				for i := 0; i < misLen-1; i++ {
					_ = c.pccMonitorIntervals.PopFront()
				}
				newSendingRate := c.sendingRate
				stepSize := float64(c.pccRounds) * PccKDecisionMadeStepSize
				if c.pccRateDirection == PccRateDirectionIncrease {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
				} else {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
				}
				c.pccRounds += 1
				c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			} else {
				c.Mode = PccSenderModePccProbing
				c.pccRounds = 1
				// Empty the monitor intervals
				for i := 0; i < misLen; i++ {
					_ = c.pccMonitorIntervals.PopFront()
				}
				c.pccMonitorIntervalNumUseful = 0
				c.pccCentralSendingRate = mis[misLen-2].SendingRate
				// Buat MI baru untuk Probing
				newSendingRate := c.pccCentralSendingRate
				stepSize := float64(c.pccRounds) * PccKProbingStepSize
				if rand.Intn(2) == 0 {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
				} else {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
				}
				c.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			}
		}
	}
	// Set Sending Rate Terbaru
	nextMi, _ := c.pccMonitorIntervals.Back()
	if c.Mode != PccSenderModePccProbing {
		c.pccCentralSendingRate = nextMi.SendingRate
	}
	c.sendingRate = nextMi.SendingRate * 1
	c.lastTimeChangeSendingRate = time.Now()
	// fmt.Println(c.Mode, c.sendingRate, c.pccCentralSendingRate)
	// fmt.Println("==================")
}

func (c *ReproducedPccAllegroSender) CanSend(bytesInFlight protocol.ByteCount) bool {
	return bytesInFlight < protocol.ByteCount(c.sendingRate)
}

func (c *ReproducedPccAllegroSender) MaybeExitSlowStart() {
}

func (c *ReproducedPccAllegroSender) OnPacketAcked(
	ackedPacketNumber protocol.PacketNumber,
	ackedBytes protocol.ByteCount,
	priorInFlight protocol.ByteCount,
	eventTime time.Time,
) {
	c.OnAckedPccMonitorInterval(ackedPacketNumber, ackedBytes, eventTime)
}

func (c *ReproducedPccAllegroSender) OnPacketLost(
	packetNumber protocol.PacketNumber,
	lostBytes, priorInFlight protocol.ByteCount,
) {
	c.OnLostPccMonitorInterval(packetNumber, lostBytes)
}

// BandwidthEstimate returns the current bandwidth estimate
func (c *ReproducedPccAllegroSender) BandwidthEstimate() Bandwidth {
	// 4/5 is pace normalizer
	return Bandwidth(c.sendingRate * 4 / 5)
}

// OnRetransmissionTimeout is called on an retransmission timeout
func (c *ReproducedPccAllegroSender) OnRetransmissionTimeout(packetsRetransmitted bool) {
}

func (c *ReproducedPccAllegroSender) SetMaxDatagramSize(s protocol.ByteCount) {
	if s < c.maxDatagramSize {
		panic(fmt.Sprintf("congestion BUG: decreased max datagram size from %d to %d", c.maxDatagramSize, s))
	}
	c.maxDatagramSize = s
	c.pacer.SetMaxDatagramSize(s)
}

func (c *ReproducedPccAllegroSender) InSlowStart() bool {
	return c.Mode == PccSenderModeStarting
}

func (c *ReproducedPccAllegroSender) InRecovery() bool {
	return false
}

// https://cseweb.ucsd.edu/classes/wi01/cse222/papers/mathis-tcpmodel-ccr97.pdf
func (c *ReproducedPccAllegroSender) GetCongestionWindow() protocol.ByteCount {
	rtt := c.rttStats.SmoothedRTT().Seconds()
	bw := float64(c.sendingRate)
	cwndBits := bw * rtt
	cwnd := protocol.ByteCount(cwndBits / 8)
	return cwnd
}

func (c *ReproducedPccAllegroSender) isEndOfInterval(nRttInterval float64) bool {
	intervalThreshold := PccKInitialRTT
	if c.pccIntervalRTTEstimator.EstimatedRTT() > 0 {
		intervalThreshold = time.Duration(nRttInterval * float64(c.pccIntervalRTTEstimator.EstimatedRTT()))
	}
	if time.Since(c.lastTimeChangeSendingRate) >= intervalThreshold {
		return true
	}
	return false
}

func (c *ReproducedPccAllegroSender) AddNewPccMonitorInterval(
	sentTime time.Time,
	packetNumber protocol.PacketNumber,
	bytes protocol.ByteCount,
	sendingRate Bandwidth,
) {
	rttVal := PccKInitialRTT
	isUseful := false
	if c.rttStats.SmoothedRTT() > 0 {
		rttVal = c.rttStats.LatestRTT()
		isUseful = true
	}
	c.pccMonitorIntervals.PushBack(&PccMonitorInterval{
		SendingRate:                  sendingRate,
		IsUseful:                     isUseful,
		RttFluctuationToleranceRatio: PccKRttFluctuationToleranceRatio,
		FirstPacketSentTime:          sentTime,
		LastPacketSentTime:           sentTime,
		FirstPacketNumber:            packetNumber,
		LastPacketNumber:             packetNumber,
		BytesSent:                    bytes,
		BytesAcked:                   0,
		BytesLost:                    0,
		RttOnMonitorStart:            rttVal,
		RttOnMonitorEnd:              rttVal,
	})
	c.pccMonitorIntervalNumUseful += 1
}

func (c *ReproducedPccAllegroSender) OnSentPccMonitorInterval(
	sentTime time.Time,
	packetNumber protocol.PacketNumber,
	bytes protocol.ByteCount,
) {
	mi, _ := c.pccMonitorIntervals.Back()
	if !(packetNumber >= mi.FirstPacketNumber) {
		return
	}
	mi.LastPacketSentTime = sentTime
	mi.LastPacketNumber = packetNumber
	mi.BytesSent += bytes
	mi.RttOnMonitorEnd = c.rttStats.LatestRTT()
}

func (c *ReproducedPccAllegroSender) OnAckedPccMonitorInterval(
	packetNumber protocol.PacketNumber,
	bytes protocol.ByteCount,
	eventTime time.Time,
) {
	mi, _ := c.pccMonitorIntervals.Back()
	if !(packetNumber >= mi.FirstPacketNumber) ||
		!(packetNumber <= mi.LastPacketNumber) {
		return
	}
	mi.BytesAcked += bytes
	mi.LastPacketAckedTime = eventTime
	if mi.FirstPacketAckedTime.IsZero() {
		mi.FirstPacketAckedTime = eventTime
	}
	c.pccIntervalRTTEstimator.AddRTTSample(c.rttStats.LatestRTT(), eventTime)
}

func (c *ReproducedPccAllegroSender) OnLostPccMonitorInterval(
	packetNumber protocol.PacketNumber,
	bytes protocol.ByteCount,
) {
	mi, _ := c.pccMonitorIntervals.Back()
	if !(packetNumber >= mi.FirstPacketNumber) ||
		!(packetNumber <= mi.LastPacketNumber) {
		return
	}
	mi.BytesLost += bytes
	mi.LastPacketAckedTime = time.Now() // Since no event time passed, assume now.
}

func CalculateUtilityAllegroV1(mi *PccMonitorInterval) float64 {
	intervalDurationSent := mi.LastPacketSentTime.Sub(mi.FirstPacketSentTime)
	// Add transfer time of the last packet sent.
	intervalDurationSent += time.Duration(time.Duration(float64(PccKConstantInitialMaxDatagramSize)/float64(mi.SendingRate)) * time.Second)
	intervalDurationAcked := mi.LastPacketAckedTime.Sub(mi.FirstPacketAckedTime)

	rttRatio := float64(mi.RttOnMonitorStart) / float64(mi.RttOnMonitorEnd)
	if (rttRatio > 1.0-mi.RttFluctuationToleranceRatio) && (rttRatio < 1.0+mi.RttFluctuationToleranceRatio) {
		rttRatio = 1.0
	}

	lossRate := float64(mi.BytesLost) / float64(mi.BytesSent)

	// latencyPenalty := 1.0 - 1.0/(1.0+math.Exp(pccKRTTCoefficient*(1.0-rttRatio)))
	// lossPenalty := 1.0 - 1.0/(1.0+math.Exp(pccKLossCoefficient*(lossRate-pccKLossTolerance)))

	tputSent := float64((mi.BytesSent-PccKConstantInitialMaxDatagramSize)*8) / intervalDurationSent.Seconds()
	tputAcked := float64(mi.BytesAcked*8) / intervalDurationAcked.Seconds()
	tputLoss := float64(mi.BytesLost*8) / intervalDurationAcked.Seconds()
	if math.IsNaN(tputLoss) {
		tputLoss = 0
	}
	tput := math.Min(tputSent, tputAcked)
	// To avoid the effect of bursty traffic by pacer
	tput = math.Min(tput, float64(mi.SendingRate))

	// return (tput*lossPenalty*latencyPenalty - tputLoss)
	// Allegro NSDI'2015 page 398 Left Top
	utilityValue := tput*sigmoidAlphaFunc(lossRate-pccKLossTolerance) - tputLoss
	// fmt.Printf("sendingRate: %.3f, rttRatio: %.3f, lossRate: %.3f, utiltiy: %.3f\n", float64(mi.SendingRate)/1e6, rttRatio, lossRate, utilityValue)
	return utilityValue
	// return tput / 1000
}

func sigmoidAlphaFunc(x float64) float64 {
	return 1. / (1 + math.Exp(-pccKLossCoefficient*x))
}

func NewIntervalRTTEstimator(t int, rtt *utils.RTTStats) IntervalRTTEstimator {
	switch t {
	case IntervalRTTSmoothedEstimatorType:
		return NewIntervalRTTSmoothedEstimator(rtt)
	case IntervalRTTJacobsonKarelEstimatorType:
		return NewIntervalRTTJacobsonKarelEstimator(rtt)
	case IntervalRTTMinFilterEstimatorType:
		return NewIntervalRTTMinFilterEstimator(rtt)
	default:
		return NewIntervalRTTSmoothedEstimator(rtt)
	}
}

type IntervalRTTEstimator interface {
	EstimatedRTT() time.Duration
	AddRTTSample(currentRTT time.Duration, eventTime time.Time)
}

type IntervalRTTSmoothedEstimator struct {
	rttStats *utils.RTTStats
}

func NewIntervalRTTSmoothedEstimator(rttStats *utils.RTTStats) *IntervalRTTSmoothedEstimator {
	intervalRTTEstimator := &IntervalRTTSmoothedEstimator{
		rttStats: rttStats,
	}
	return intervalRTTEstimator
}

func (i *IntervalRTTSmoothedEstimator) EstimatedRTT() time.Duration {
	return i.rttStats.SmoothedRTT()
}

func (i *IntervalRTTSmoothedEstimator) AddRTTSample(currentRTT time.Duration, eventTime time.Time) {}

type IntervalRTTJacobsonKarelEstimator struct {
	rttStats *utils.RTTStats
}

func NewIntervalRTTJacobsonKarelEstimator(rttStats *utils.RTTStats) *IntervalRTTJacobsonKarelEstimator {
	intervalRTTEstimator := &IntervalRTTJacobsonKarelEstimator{rttStats: rttStats}
	return intervalRTTEstimator
}

func (i *IntervalRTTJacobsonKarelEstimator) EstimatedRTT() time.Duration {
	return i.rttStats.SmoothedRTT() + utils.Max(4*i.rttStats.MeanDeviation(), protocol.TimerGranularity)
}

func (i *IntervalRTTJacobsonKarelEstimator) AddRTTSample(currentRTT time.Duration, eventTime time.Time) {
}

type RTTSeries struct {
	EventTime time.Time
	Val       time.Duration
}

type IntervalRTTMinFilterEstimator struct {
	vals *deque.Deque[RTTSeries]
	// extreme time.Duration
}

// Inspired by Copa
// https://github.com/venkatarun95/genericCC/blob/master/rtt-window.cc
func NewIntervalRTTMinFilterEstimator(rttStats *utils.RTTStats) *IntervalRTTMinFilterEstimator {
	intervalRTTEstimator := &IntervalRTTMinFilterEstimator{}
	intervalRTTEstimator.vals = deque.NewDeque[RTTSeries]()
	return intervalRTTEstimator
}

func (i *IntervalRTTMinFilterEstimator) EstimatedRTT() time.Duration {
	if val, ok := i.vals.Front(); ok {
		return val.Val
	}
	return 0
}

func (i *IntervalRTTMinFilterEstimator) AddRTTSample(currentRTT time.Duration, eventTime time.Time) {
	for !i.vals.IsEmpty() {
		if val, _ := i.vals.Back(); val.Val > currentRTT {
			_ = i.vals.PopBack()
		} else {
			break
		}
	}
	i.vals.PushBack(RTTSeries{eventTime, currentRTT})
	i.clearOldHistory(eventTime)
}

func (i *IntervalRTTMinFilterEstimator) clearOldHistory(eventTime time.Time) {
	for i.vals.Len() > 1 {
		val, _ := i.vals.Front()
		if val.EventTime.Before(eventTime.Add(-10 * time.Second)) {
			_ = i.vals.PopFront()
		} else {
			break
		}
	}
}
