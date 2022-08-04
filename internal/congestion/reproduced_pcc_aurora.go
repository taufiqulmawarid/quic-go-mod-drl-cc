package congestion

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	deque "github.com/edwingeng/deque/v2"
	python3 "github.com/go-python/cpy3"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/logging"
)

const (
	pyAuroraPath                   = "py_aurora/testing"
	pyFilename                     = "loaded_client"
	PccKAuroraNRttInterval float64 = 1.5
)

var tfModelPath string = "py_aurora/saved_models/icml_paper_model"

// PccPythonRateController Singleton
var pccAuroralock = &sync.Mutex{}
var pccPythonRateController *PccPythonRateController

type ReproducedPccAuroraSender struct {
	pcc              *ReproducedPccAllegroSender
	pyRateController *PccPythonRateController

	UseSlowStart             bool
	hybridSlowStart          HybridSlowStart
	largestSentPacketNumber  protocol.PacketNumber
	largestAckedPacketNumber protocol.PacketNumber
}

func NewReproducedPccAuroraSender(
	rttStats *utils.RTTStats,
	initialMaxDatagramSize protocol.ByteCount,
	useSlowStart bool,
	tracer logging.ConnectionTracer,
) *ReproducedPccAuroraSender {
	return newReproducedPccAuroraSender(
		rttStats,
		initialMaxDatagramSize,
		initialCongestionWindow*initialMaxDatagramSize,
		protocol.MaxCongestionWindowPackets*initialMaxDatagramSize,
		useSlowStart,
		tracer,
	)
}

func newReproducedPccAuroraSender(
	rttStats *utils.RTTStats,
	initialMaxDatagramSize,
	initialCongestionWindow,
	initialMaxCongestionWindow protocol.ByteCount,
	useSlowStart bool,
	tracer logging.ConnectionTracer,
) *ReproducedPccAuroraSender {
	for i, ivar := range os.Args {
		if ivar == "-aurora-model" {
			tfModelPath = os.Args[i+1]
			break
		}
	}
	c := &ReproducedPccAuroraSender{
		pcc: &ReproducedPccAllegroSender{
			rttStats:        rttStats,
			tracer:          tracer,
			maxDatagramSize: initialMaxDatagramSize,
		},
		UseSlowStart: useSlowStart,
	}

	// Configuring Starting Mode
	if c.UseSlowStart {
		c.pcc.Mode = PccSenderModeStarting
	} else {
		c.pcc.Mode = PccSenderModePccProbing
	}
	c.pcc.pccRateDirection = PccRateDirectionIncrease
	c.pcc.lastTimeChangeSendingRate = time.Now()
	c.pcc.pccRounds = 1

	// Aurora component
	callFreq := 1. / PccNRttInterval
	auroraRateController, err := NewPccPythonRateController(callFreq)
	if err != nil {
		panic(fmt.Sprintf("Unable to initiate aurora rate controller: %s", err))
	}
	c.pyRateController = auroraRateController
	newSendingRate, err := c.pyRateController.GetNextSendingRate()
	if err != nil {
		panic("Unable to get new sending rate")
	}
	c.pcc.pccCentralSendingRate = newSendingRate
	c.pcc.sendingRate = c.pcc.pccCentralSendingRate * 1

	// Create initial PCC Monitor Interval
	c.pcc.pccMonitorIntervals = deque.NewDeque[*PccMonitorInterval]()
	c.pcc.pccMonitorIntervals.PushBack(
		&PccMonitorInterval{
			SendingRate:                  c.pcc.sendingRate,
			IsUseful:                     false,
			RttFluctuationToleranceRatio: PccKRttFluctuationToleranceRatio,
			RttOnMonitorStart:            PccKInitialRTT,
		},
	)

	c.pcc.pacer = newPacer(c.pcc.BandwidthEstimate)

	if c.pcc.tracer != nil {
		c.pcc.lastState = logging.CongestionStateSlowStart
		c.pcc.tracer.UpdatedCongestionState(logging.CongestionStateSlowStart)
	}
	return c
}

// TimeUntilSend returns when the next packet should be sent.
func (c *ReproducedPccAuroraSender) TimeUntilSend(packetSize protocol.ByteCount) time.Time {
	return c.pcc.TimeUntilSend(packetSize)
}

func (c *ReproducedPccAuroraSender) HasPacingBudget() bool {
	return c.pcc.HasPacingBudget()
}

func (c *ReproducedPccAuroraSender) OnPacketSent(
	sentTime time.Time,
	bytesInFlight protocol.ByteCount,
	packetNumber protocol.PacketNumber,
	bytes protocol.ByteCount,
	isRetransmittable bool,
) {
	c.pcc.pacer.SentPacket(sentTime, bytes)
	if !isRetransmittable {
		return
	}
	// To support aurora with slow start experiment
	c.largestSentPacketNumber = packetNumber
	c.hybridSlowStart.OnPacketSent(packetNumber)

	// Lines below are Aurora's mechanics
	isEndOfInterval := c.isEndOfInterval(PccKAuroraNRttInterval)
	isMiEmpty := c.pcc.pccMonitorIntervals.IsEmpty()
	// Make sure that the PCC Monitor Interval is not empty
	if isMiEmpty {
		panic("The PCC Monitor Interval is Empty")
	}
	// Update the monitor interval when:
	// 1. Not at the end of interval
	// 2. Monitor interval is not empty
	if !isEndOfInterval && !isMiEmpty {
		c.pcc.OnSentPccMonitorInterval(sentTime, packetNumber, bytes)
		return
	}
	if mi, ok := c.pcc.pccMonitorIntervals.Back(); ok {
		if mi.IsUseful {
			c.pyRateController.MonitorIntervalFinished(mi)
		}
	}
	if c.pcc.Mode == PccSenderModeStarting {
		if c.UseSlowStart {
			c.pcc.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, c.pcc.sendingRate)
			return
		}
		if c.pcc.pccMonitorIntervalNumUseful < 2 {
			// Buat MI Baru
			newSendingRate := Bandwidth(.5 * float64(c.pcc.sendingRate))
			if c.pcc.pccMonitorIntervalNumUseful == 1 {
				newSendingRate = Bandwidth(2. * float64(c.pcc.sendingRate))
			}
			c.pcc.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
		} else {
			// Calculate Utility Value
			mis := c.pcc.pccMonitorIntervals.Dump()
			misLen := len(mis)
			if misLen < 2 {
				panic("monitor interval is less than 2")
			}
			PastUtilityValue := CalculateUtilityAllegroV1(mis[misLen-2])
			CurrentUtilityValue := CalculateUtilityAllegroV1(mis[misLen-1])
			if CurrentUtilityValue >= PastUtilityValue {
				for i := 0; i < misLen-1; i++ {
					_ = c.pcc.pccMonitorIntervals.PopFront()
				}
				newSendingRate := Bandwidth(2. * float64(c.pcc.sendingRate))
				c.pcc.pccRounds += 1
				c.pcc.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			} else {
				c.pcc.Mode = PccSenderModePccProbing
				c.pcc.pccRounds = 1
				// Empty the monitor intervals
				for i := 0; i < misLen; i++ {
					_ = c.pcc.pccMonitorIntervals.PopFront()
				}
				c.pcc.pccMonitorIntervalNumUseful = 0
				c.pcc.pccCentralSendingRate = mis[misLen-2].SendingRate
				// Buat MI baru untuk Probing
				newSendingRate := c.pcc.pccCentralSendingRate
				stepSize := float64(c.pcc.pccRounds) * PccKProbingStepSize
				if rand.Intn(2) == 0 {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 + stepSize))
				} else {
					newSendingRate = Bandwidth(float64(newSendingRate) * (1.0 - stepSize))
				}
				c.pcc.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
			}
		}
	} else {
		// Acquire new sending rate from model
		newSendingRate, err := c.pyRateController.GetNextSendingRate()
		if err != nil {
			panic("Unable to get new sending rate")
		}
		// Clear and Buat MI Baru
		misLen := c.pcc.pccMonitorIntervals.Len()
		for i := 0; i < misLen; i++ {
			_ = c.pcc.pccMonitorIntervals.PopFront()
		}
		c.pcc.AddNewPccMonitorInterval(sentTime, packetNumber, bytes, newSendingRate)
	}
	// Set Sending Rate Terbaru
	nextMi, _ := c.pcc.pccMonitorIntervals.Back()
	if c.pcc.Mode != PccSenderModePccProbing {
		c.pcc.pccCentralSendingRate = nextMi.SendingRate
	}
	c.pcc.sendingRate = nextMi.SendingRate * 1
	c.pcc.lastTimeChangeSendingRate = time.Now()
	// fmt.Println(c.pcc.Mode, c.pcc.sendingRate, c.pcc.pccCentralSendingRate)
	// fmt.Println("==================")
}

func (c *ReproducedPccAuroraSender) CanSend(bytesInFlight protocol.ByteCount) bool {
	return c.pcc.CanSend(bytesInFlight)
}

func (c *ReproducedPccAuroraSender) MaybeExitSlowStart() {
	// if c.InSlowStart() &&
	// 	c.hybridSlowStart.ShouldExitSlowStart(c.pcc.rttStats.LatestRTT(), c.pcc.rttStats.MinRTT(), c.GetCongestionWindow()/c.pcc.maxDatagramSize) {
	// 	// exit slow start
	// 	c.pcc.Mode = PccSenderModePccProbing
	// }
}

func (c *ReproducedPccAuroraSender) OnPacketAcked(
	ackedPacketNumber protocol.PacketNumber,
	ackedBytes protocol.ByteCount,
	priorInFlight protocol.ByteCount,
	eventTime time.Time,
) {
	c.pcc.OnPacketAcked(ackedPacketNumber, ackedBytes, priorInFlight, eventTime)

	// Slow start
	if c.pcc.Mode == PccSenderModeStarting {
		c.largestAckedPacketNumber = utils.MaxPacketNumber(ackedPacketNumber, c.largestAckedPacketNumber)
		if c.InRecovery() {
			return
		}
		c.maybeIncreaseRate(ackedPacketNumber, ackedBytes, priorInFlight, eventTime)
		if c.InSlowStart() {
			c.hybridSlowStart.OnPacketAcked(ackedPacketNumber)
		}
	}
}

func (c *ReproducedPccAuroraSender) OnPacketLost(
	packetNumber protocol.PacketNumber,
	lostBytes, priorInFlight protocol.ByteCount,
) {
	c.pcc.OnPacketLost(packetNumber, lostBytes, priorInFlight)

	// Exit slow start
	if c.pcc.Mode == PccSenderModeStarting {
		c.pcc.sendingRate = Bandwidth(float64(c.pcc.sendingRate) * renoBeta)
		c.pcc.Mode = PccSenderModePccProbing
	}
}

// OnRetransmissionTimeout is called on an retransmission timeout
func (c *ReproducedPccAuroraSender) OnRetransmissionTimeout(packetsRetransmitted bool) {
}

func (c *ReproducedPccAuroraSender) SetMaxDatagramSize(s protocol.ByteCount) {
	c.pcc.SetMaxDatagramSize(s)
}

func (c *ReproducedPccAuroraSender) InSlowStart() bool {
	return c.pcc.InSlowStart()
}

func (c *ReproducedPccAuroraSender) InRecovery() bool {
	return false
}

func (c *ReproducedPccAuroraSender) GetCongestionWindow() protocol.ByteCount {
	return c.pcc.GetCongestionWindow()
}

func (c *ReproducedPccAuroraSender) isEndOfInterval(nRttInterval float64) bool {
	return c.pcc.isEndOfInterval(nRttInterval)
}

// Called when we receive an ack. Normal TCP tracks how many packets one ack
// represents, but quic has a separate ack for each packet.
func (c *ReproducedPccAuroraSender) maybeIncreaseRate(
	_ protocol.PacketNumber,
	ackedBytes protocol.ByteCount,
	priorInFlight protocol.ByteCount,
	eventTime time.Time,
) {
	// Do not increase the congestion window unless the sender is close to using
	// the current window.
	if !c.isCwndLimited(priorInFlight) {
		return
	}
	if c.InSlowStart() {
		// TCP slow start, exponential growth, increase by one for each ACK.
		c.pcc.sendingRate += Bandwidth(c.pcc.maxDatagramSize * 8)
		currentMi, _ := c.pcc.pccMonitorIntervals.Back()
		currentMi.SendingRate = c.pcc.sendingRate
		c.pcc.pccCentralSendingRate = c.pcc.sendingRate
		return
	}
}

func (c *ReproducedPccAuroraSender) isCwndLimited(bytesInFlight protocol.ByteCount) bool {
	congestionWindow := c.GetCongestionWindow()
	if bytesInFlight >= congestionWindow {
		return true
	}
	availableBytes := congestionWindow - bytesInFlight
	slowStartLimited := c.InSlowStart() && bytesInFlight > congestionWindow/2
	return slowStartLimited || availableBytes <= maxBurstPackets*c.pcc.maxDatagramSize
}

type PccPythonRateController struct {
	pythonInitialized bool
	id                int
	hasTimeOffset     bool
	timeOffset        time.Time
	module            *python3.PyObject
	give_sample_func  *python3.PyObject
	get_rate_func     *python3.PyObject
	reset_func        *python3.PyObject
}

func NewPccPythonRateController(callFreq float64) (*PccPythonRateController, error) {
	pccAuroralock.Lock()
	defer pccAuroralock.Unlock()
	if pccPythonRateController == nil {
		pccPythonRateController = &PccPythonRateController{}
	}
	if !pccPythonRateController.pythonInitialized {
		err := pccPythonRateController.InitializePython()
		if err != nil {
			return nil, err
		}
	}

	pccPythonRateController.id = pccPythonRateController.GetNextId()
	pccPythonRateController.hasTimeOffset = false
	pccPythonRateController.timeOffset = time.Time{}
	command := fmt.Sprintf("sys.path.append(%q)", pyAuroraPath)
	python3.PyRun_SimpleString(command)

	pccPythonRateController.module = python3.PyImport_ImportModule(pyFilename)
	if pccPythonRateController.module == nil {
		python3.PyErr_Print()
		return nil, fmt.Errorf("ERROR: Could not load python module: %s", pyFilename)
	}

	initFunc := pccPythonRateController.module.GetAttrString("init")
	if initFunc == nil {
		python3.PyErr_Print()
		return nil, errors.New("ERROR: Could not load python function: init")
	}
	idObj := python3.PyLong_FromGoInt(pccPythonRateController.id)
	args := python3.PyTuple_New(1)
	python3.PyTuple_SetItem(args, 0, idObj)
	initResult := initFunc.CallObject(args)
	if initResult == nil {
		python3.PyErr_Print()
		return nil, errors.New("ERROR: Could not run python function: init")
	}

	pccPythonRateController.give_sample_func = pccPythonRateController.module.GetAttrString("give_sample")
	if pccPythonRateController.give_sample_func == nil {
		python3.PyErr_Print()
		return nil, errors.New("ERROR: Could not load python function: give_sample")
	}

	pccPythonRateController.get_rate_func = pccPythonRateController.module.GetAttrString("get_rate")
	if pccPythonRateController.get_rate_func == nil {
		python3.PyErr_Print()
		return nil, errors.New("ERROR: Could not load python function: get_rate")
	}

	pccPythonRateController.reset_func = pccPythonRateController.module.GetAttrString("reset")
	if pccPythonRateController.reset_func == nil {
		python3.PyErr_Print()
		return nil, errors.New("ERROR: Could not load python function: reset")
	}

	return pccPythonRateController, nil
}

func (r *PccPythonRateController) GetNextSendingRate() (Bandwidth, error) {
	pccAuroralock.Lock()
	defer pccAuroralock.Unlock()
	idObj := python3.PyLong_FromGoInt(r.id)
	args := python3.PyTuple_New(1)
	python3.PyTuple_SetItem(args, 0, idObj)
	result := r.get_rate_func.CallObject(args)
	if result == nil {
		python3.PyErr_Print()
		return 0, fmt.Errorf("ERROR: Failed to call python get_rate() func")
	}
	if !python3.PyFloat_Check(result) {
		return 0, fmt.Errorf("ERROR: Output from python get_rate() is not a float")
	}
	resultFloat64 := python3.PyFloat_AsDouble(result)
	python3.PyErr_Print()
	result.DecRef()
	return Bandwidth(resultFloat64), nil
}

func (r *PccPythonRateController) MonitorIntervalFinished(mi *PccMonitorInterval) {
	if !r.hasTimeOffset {
		r.timeOffset = mi.FirstPacketSentTime
		r.hasTimeOffset = true
	}
	r.GiveSample(
		mi.SendingRate,
		mi.FirstPacketSentTime.Sub(r.timeOffset).Seconds(),
		mi.LastPacketSentTime.Sub(r.timeOffset).Seconds(),
		mi.BytesSent,
		mi.BytesAcked,
		mi.BytesLost,
		mi.RttOnMonitorStart.Seconds(),
		mi.RttOnMonitorEnd.Seconds(),
		mi.FirstPacketAckedTime.Sub(r.timeOffset).Seconds(),
		mi.LastPacketAckedTime.Sub(r.timeOffset).Seconds(),
		PccKConstantInitialMaxDatagramSize,
		CalculateUtilityAllegroV1(mi),
	)
}

func (r *PccPythonRateController) Reset() {
	pccAuroralock.Lock()
	defer pccAuroralock.Unlock()
	idObj := python3.PyLong_FromGoInt(r.id)
	args := python3.PyTuple_New(1)
	python3.PyTuple_SetItem(args, 0, idObj)
	_ = r.reset_func.CallObject(args)
	python3.PyErr_Print()
}

func (r *PccPythonRateController) InitializePython() error {
	python3.Py_Initialize()
	python3.PyRun_SimpleString("import sys")
	pyArgv := fmt.Sprintf("sys.argv = ['--model-path=%s']", tfModelPath)
	exitCode := python3.PyRun_SimpleString(pyArgv)
	if exitCode != 0 {
		return errors.New("InitializePython is unable to pass arguments")
	}
	r.pythonInitialized = true
	return nil
}

func (r *PccPythonRateController) GetNextId() int {
	return r.id + 1
}

func (r *PccPythonRateController) GiveSample(
	sendingRate Bandwidth,
	firstPacketSentTime float64,
	lastPacketSentTime float64,
	bytesSent protocol.ByteCount,
	bytesAcked protocol.ByteCount,
	bytesLost protocol.ByteCount,
	rttOnMonitorStart float64,
	rttOnMonitorEnd float64,
	firstPacketAckedTime float64,
	lastPacketAckedTime float64,
	packetSize protocol.ByteCount,
	utility float64,
) {
	pccAuroralock.Lock()
	defer pccAuroralock.Unlock()
	args := python3.PyTuple_New(11)
	python3.PyTuple_SetItem(args, 0, python3.PyLong_FromGoInt(r.id))
	python3.PyTuple_SetItem(args, 1, python3.PyLong_FromGoInt64(int64(bytesSent)))
	python3.PyTuple_SetItem(args, 2, python3.PyLong_FromGoInt64(int64(bytesAcked)))
	python3.PyTuple_SetItem(args, 3, python3.PyLong_FromGoInt64(int64(bytesLost)))
	python3.PyTuple_SetItem(args, 4, python3.PyFloat_FromDouble(firstPacketSentTime))
	python3.PyTuple_SetItem(args, 5, python3.PyFloat_FromDouble(lastPacketSentTime))
	python3.PyTuple_SetItem(args, 6, python3.PyFloat_FromDouble(firstPacketAckedTime))
	python3.PyTuple_SetItem(args, 7, python3.PyFloat_FromDouble(lastPacketAckedTime))
	rttSamples := python3.PyList_New(2)
	python3.PyList_SetItem(rttSamples, 0, python3.PyFloat_FromDouble(rttOnMonitorStart))
	python3.PyList_SetItem(rttSamples, 1, python3.PyFloat_FromDouble(rttOnMonitorEnd))
	python3.PyTuple_SetItem(args, 8, rttSamples)
	python3.PyTuple_SetItem(args, 9, python3.PyLong_FromGoInt64(int64(packetSize)))
	python3.PyTuple_SetItem(args, 10, python3.PyFloat_FromDouble(utility))
	r.give_sample_func.CallObject(args)
	python3.PyErr_Print()
}
