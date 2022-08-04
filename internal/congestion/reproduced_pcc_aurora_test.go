package congestion

import (
	"testing"
	"time"

	"github.com/lucas-clemente/quic-go/internal/protocol"
)

func TestNewPccPythonRateController(t *testing.T) {
	callFreq := 0.25 // of the RTT
	pccPythonRateController, err := NewPccPythonRateController(callFreq)
	if err != nil {
		t.Errorf(err.Error())
	}
	if pccPythonRateController == nil {
		t.Errorf("pccPythonRateController should exists")
	}
}

func TestPccPythonRateControllerReset(t *testing.T) {
	callFreq := 0.25 // of the RTT
	pccPythonRateController, _ := NewPccPythonRateController(callFreq)
	pccPythonRateController.Reset()
}

func TestPccPythonRateControllerGiveSample(t *testing.T) {
	callFreq := 0.25 // of the RTT
	pccPythonRateController, _ := NewPccPythonRateController(callFreq)
	pccPythonRateController.GiveSample(
		Bandwidth(1000000),
		100,
		160,
		protocol.ByteCount(80*1000),
		protocol.ByteCount(60*1000),
		protocol.ByteCount(0),
		0.15,
		0.3,
		101,
		165,
		PccKConstantInitialMaxDatagramSize,
		0.001,
	)
}

func TestPccPythonRateControllerMonitorIntervalFinished(t *testing.T) {
	callFreq := 0.25 // of the RTT
	pccPythonRateController, _ := NewPccPythonRateController(callFreq)
	mi := &PccMonitorInterval{
		SendingRate:                  Bandwidth(1000000),
		IsUseful:                     true,
		RttFluctuationToleranceRatio: PccKRttFluctuationToleranceRatio,
		FirstPacketSentTime:          time.UnixMilli(100),
		LastPacketSentTime:           time.UnixMilli(160),
		FirstPacketNumber:            protocol.PacketNumber(100),
		LastPacketNumber:             protocol.PacketNumber(221),
		BytesSent:                    protocol.ByteCount(80 * 1000),
		BytesAcked:                   protocol.ByteCount(60 * 1000),
		BytesLost:                    protocol.ByteCount(0),
		RttOnMonitorStart:            time.Duration(150 * time.Millisecond),
		RttOnMonitorEnd:              time.Duration(30 * time.Millisecond),
		FirstPacketAckedTime:         time.UnixMilli(101),
		LastPacketAckedTime:          time.UnixMilli(165),
	}
	pccPythonRateController.MonitorIntervalFinished(mi)
}

func TestPccPythonRateControllerGetNextSendingRate(t *testing.T) {
	callFreq := 0.25 // of the RTT
	pccPythonRateController, _ := NewPccPythonRateController(callFreq)
	mi := &PccMonitorInterval{
		SendingRate:                  Bandwidth(1000000),
		IsUseful:                     true,
		RttFluctuationToleranceRatio: PccKRttFluctuationToleranceRatio,
		FirstPacketSentTime:          time.UnixMilli(100),
		LastPacketSentTime:           time.UnixMilli(160),
		FirstPacketNumber:            protocol.PacketNumber(100),
		LastPacketNumber:             protocol.PacketNumber(221),
		BytesSent:                    protocol.ByteCount(80 * 1000),
		BytesAcked:                   protocol.ByteCount(60 * 1000),
		BytesLost:                    protocol.ByteCount(0),
		RttOnMonitorStart:            time.Duration(150 * time.Millisecond),
		RttOnMonitorEnd:              time.Duration(30 * time.Millisecond),
		FirstPacketAckedTime:         time.UnixMilli(101),
		LastPacketAckedTime:          time.UnixMilli(165),
	}
	pccPythonRateController.MonitorIntervalFinished(mi)
	result, err := pccPythonRateController.GetNextSendingRate()
	if err != nil {
		t.Fatalf("Unable to get next sending rate")
	}
	if result == 0 {
		t.Fatalf("get next sending rate result is 0")
	}
}
