// Package modbusclient provides modbus Serial Line/RTU and TCP/IP access
// for client (master) applications to communicate with server (slave)
// devices. Logic specifically in this file implements the Serial Line/RTU
// protocol.
package modbusclient

import (
	"fmt"
	"github.com/tarm/serial"
	"io"
	"log"
	"time"
)

type RTUConfig struct {
	Serial       serial.Config
	SlaveAddress byte
	Timeout      time.Duration
	Debug        bool
}

type RTUContext struct {
	*serial.Port
	RTUConfig
}

// crc computes and returns a cyclic redundancy check of the given byte array
func crc(data []byte) uint16 {
	var crc16 uint16 = 0xffff
	l := len(data)
	for i := 0; i < l; i++ {
		crc16 ^= uint16(data[i])
		for j := 0; j < 8; j++ {
			if crc16&0x0001 > 0 {
				crc16 = (crc16 >> 1) ^ 0xA001
			} else {
				crc16 >>= 1
			}
		}
	}
	return crc16
}

// GenerateRTUFrame is a method corresponding to a RTUFrame object which
// returns a byte array representing the associated serial line/RTU
// application data unit (ADU)
func (frame *RTUFrame) GenerateRTUFrame() []byte {

	insertNumOfRegister := true
	insertDataLen := false
	switch frame.FunctionCode {
	case FUNCTION_WRITE_SINGLE_COIL, FUNCTION_WRITE_SINGLE_REGISTER:
		insertNumOfRegister = false
	case FUNCTION_WRITE_MULTIPLE_REGISTERS:
		insertDataLen = true
	}
	packetLen := 9
	dataLen := len(frame.Data)
	if dataLen > 0 {
		packetLen = RTU_FRAME_MAXSIZE
	}

	packet := make([]byte, packetLen)
	packet[0] = frame.SlaveAddress
	packet[1] = frame.FunctionCode
	packet[2] = byte(frame.StartRegister >> 8)   // (High Byte)
	packet[3] = byte(frame.StartRegister & 0xff) // (Low Byte)
	bytesUsed := 4
	if insertNumOfRegister {
		packet[bytesUsed] = byte(frame.NumberOfRegisters >> 8)         // (High Byte)
		packet[(bytesUsed + 1)] = byte(frame.NumberOfRegisters & 0xff) // (Low Byte)
		bytesUsed += 2
	}
	if insertDataLen {
		packet[bytesUsed] = byte(dataLen)
		bytesUsed++
	}

	for i := 0; i < dataLen; i++ {
		packet[(bytesUsed + i)] = frame.Data[i]
	}
	bytesUsed += dataLen

	// add the crc to the end
	packetCrc := crc(packet[:bytesUsed])
	packet[bytesUsed] = byte(packetCrc & 0xff)
	packet[(bytesUsed + 1)] = byte(packetCrc >> 8)
	bytesUsed += 2

	return packet[:bytesUsed]
}

// ConnectRTU attempts to access the Serial Device for subsequent
// RTU writes and response reads from the modbus slave device
func ConnectRTU(cfg RTUConfig) (*RTUContext, error) {
	port, err := serial.OpenPort(&cfg.Serial)
	if err != nil {
		return nil, err
	}

	ctx := RTUContext{
		Port:      port,
		RTUConfig: cfg,
	}
	return &ctx, nil
}

// DisconnectRTU closes the underlying Serial Device connection
func DisconnectRTU(ctx io.ReadWriteCloser) {
	ctx.Close()
}

// viaRTU is a private method which applies the given function validator,
// to make sure the functionCode passed is valid for the operation
// desired. If correct, it creates an RTUFrame given the corresponding
// information, attempts to open the serialDevice, and if successful, transmits
// it to the modbus server (slave device) specified by the given serial connection,
// and returns a byte array of the slave device's reply, and error (if any)
func (rtu *RTUContext) viaRTU(fnValidator func(byte) bool, functionCode byte, startRegister, numRegisters uint16, data []byte) ([]byte, error) {
	if fnValidator(functionCode) {
		frame := new(RTUFrame)
		frame.SlaveAddress = rtu.SlaveAddress
		frame.FunctionCode = functionCode
		frame.StartRegister = startRegister
		frame.NumberOfRegisters = numRegisters
		if len(data) > 0 {
			frame.Data = data
		}

		// generate the ADU from the RTU frame
		adu := frame.GenerateRTUFrame()
		if rtu.Debug {
			log.Println(fmt.Sprintf("Tx: %x", adu))
		}

		// transmit the ADU to the slave device via the
		// serial port represented by the fd pointer
		_, werr := rtu.Write(adu)
		if werr != nil {
			if rtu.Debug {
				log.Println(fmt.Sprintf("RTU Write Err: %s", werr))
			}
			return []byte{}, werr
		}

		// allow the slave device adequate time to respond
		time.Sleep(rtu.Timeout)

		// then attempt to read the reply
		response := make([]byte, RTU_FRAME_MAXSIZE)
		n, rerr := rtu.Read(response)
		if rerr != nil {
			if rtu.Debug {
				log.Println(fmt.Sprintf("RTU Read Err: %s", rerr))
			}
			return []byte{}, rerr
		}

		// check the validity of the response
		if response[0] != frame.SlaveAddress || response[1] != frame.FunctionCode {
			if rtu.Debug {
				log.Println("RTU Response Invalid")
			}
			if response[0] == frame.SlaveAddress && (response[1]&0x7f) == frame.FunctionCode {
				switch response[2] {
				case EXCEPTION_ILLEGAL_FUNCTION:
					return []byte{}, MODBUS_EXCEPTIONS[EXCEPTION_ILLEGAL_FUNCTION]
				case EXCEPTION_DATA_ADDRESS:
					return []byte{}, MODBUS_EXCEPTIONS[EXCEPTION_DATA_ADDRESS]
				case EXCEPTION_DATA_VALUE:
					return []byte{}, MODBUS_EXCEPTIONS[EXCEPTION_DATA_VALUE]
				case EXCEPTION_SLAVE_DEVICE_FAILURE:
					return []byte{}, MODBUS_EXCEPTIONS[EXCEPTION_SLAVE_DEVICE_FAILURE]
				}
			}
			return []byte{}, MODBUS_EXCEPTIONS[EXCEPTION_UNSPECIFIED]
		}

		// confirm the checksum (crc)
		responseCrc := crc(response[:(n - 2)])
		if response[(n-2)] != byte((responseCrc&0xff)) ||
			response[(n-1)] != byte((responseCrc>>8)) {
			// crc failed (odd that there's no specific code for it)
			if rtu.Debug {
				log.Println("RTU Response Invalid: Bad Checksum")
			}
			// return the response bytes anyway, and let the caller decide
			return response[:n], MODBUS_EXCEPTIONS[EXCEPTION_BAD_CHECKSUM]
		}

		// return only the number of bytes read
		return response[:n], nil
	}

	return []byte{}, MODBUS_EXCEPTIONS[EXCEPTION_ILLEGAL_FUNCTION]
}

// RTURead performs the given modbus Read function over RTU to the given
// serialDevice, using the given frame data
func (rtu *RTUContext) RTURead(functionCode byte, startRegister, numRegisters uint16) ([]byte, error) {
	return rtu.viaRTU(ValidReadFunction, functionCode, startRegister, numRegisters, []byte{})
}

// RTUWrite performs the given modbus Write function over RTU to the given
// serialDevice, using the given frame data
func (rtu *RTUContext) RTUWrite(functionCode byte, startRegister, numRegisters uint16, data []byte) ([]byte, error) {
	return rtu.viaRTU(ValidWriteFunction, functionCode, startRegister, numRegisters, data)
}
