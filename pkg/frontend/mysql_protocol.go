// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package frontend

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/fagongzi/goetty/v2"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/config"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/defines"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	planPb "github.com/matrixorigin/matrixone/pkg/pb/plan"
	plan2 "github.com/matrixorigin/matrixone/pkg/sql/plan"
)

// DefaultCapability means default capabilities of the server
var DefaultCapability = CLIENT_LONG_PASSWORD |
	CLIENT_FOUND_ROWS |
	CLIENT_LONG_FLAG |
	CLIENT_CONNECT_WITH_DB |
	CLIENT_LOCAL_FILES |
	CLIENT_PROTOCOL_41 |
	CLIENT_INTERACTIVE |
	CLIENT_TRANSACTIONS |
	CLIENT_SECURE_CONNECTION |
	CLIENT_MULTI_STATEMENTS |
	CLIENT_MULTI_RESULTS |
	CLIENT_PLUGIN_AUTH |
	CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA |
	CLIENT_DEPRECATE_EOF

// DefaultClientConnStatus default server status
var DefaultClientConnStatus = SERVER_STATUS_AUTOCOMMIT

var serverVersion atomic.Value

func init() {
	serverVersion.Store("0.5.0")
}

func InitServerVersion(v string) {
	if len(v) > 0 {
		switch v[0] {
		case 'v': // format 'v1.1.1'
			v = v[1:]
			serverVersion.Store(v)
		default:
			vv := []byte(v)
			for i := 0; i < len(vv); i++ {
				if !unicode.IsDigit(rune(vv[i])) && vv[i] != '.' {
					vv = append(vv[:i], vv[i+1:]...)
					i--
				}
			}
			serverVersion.Store(string(vv))
		}
	} else {
		serverVersion.Store("0.5.0")
	}
}

const (
	clientProtocolVersion uint8 = 10

	/**
	An answer talks about the charset utf8mb4.
	https://stackoverflow.com/questions/766809/whats-the-difference-between-utf8-general-ci-and-utf8-unicode-ci
	It recommends the charset utf8mb4_0900_ai_ci.
	Maybe we can support utf8mb4_0900_ai_ci in the future.

	A concise research in the Mysql 8.0.23.

	the charset in sever level
	======================================

	mysql> show variables like 'character_set_server';
	+----------------------+---------+
	| Variable_name        | Value   |
	+----------------------+---------+
	| character_set_server | utf8mb4 |
	+----------------------+---------+

	mysql> show variables like 'collation_server';
	+------------------+--------------------+
	| Variable_name    | Value              |
	+------------------+--------------------+
	| collation_server | utf8mb4_0900_ai_ci |
	+------------------+--------------------+

	the charset in database level
	=====================================
	mysql> show variables like 'character_set_database';
	+------------------------+---------+
	| Variable_name          | Value   |
	+------------------------+---------+
	| character_set_database | utf8mb4 |
	+------------------------+---------+

	mysql> show variables like 'collation_database';
	+--------------------+--------------------+
	| Variable_name      | Value              |
	+--------------------+--------------------+
	| collation_database | utf8mb4_0900_ai_ci |
	+--------------------+--------------------+

	*/
	// DefaultCollationID is utf8mb4_bin(46)
	utf8mb4BinCollationID uint8 = 46

	Utf8mb4CollationID uint8 = 45

	AuthNativePassword string = "mysql_native_password"

	//the length of the mysql protocol header
	HeaderLengthOfTheProtocol int = 4
	HeaderOffset              int = 0

	// MaxPayloadSize If the payload is larger than or equal to 2^24−1 bytes the length is set to 2^24−1 (ff ff ff)
	//and additional packets are sent with the rest of the payload until the payload of a packet
	//is less than 2^24−1 bytes.
	MaxPayloadSize uint32 = (1 << 24) - 1

	// DefaultMySQLState is the default state of the mySQL
	DefaultMySQLState string = "HY000"
)

type MysqlProtocol interface {
	Protocol
	//the server send group row of the result set as an independent packet thread safe
	SendResultSetTextBatchRow(mrs *MysqlResultSet, cnt uint64) error

	SendResultSetTextBatchRowSpeedup(mrs *MysqlResultSet, cnt uint64) error

	//SendColumnDefinitionPacket the server send the column definition to the client
	SendColumnDefinitionPacket(ctx context.Context, column Column, cmd int) error

	//SendColumnCountPacket makes the column count packet
	SendColumnCountPacket(count uint64) error

	SendResponse(ctx context.Context, resp *Response) error

	SendEOFPacketIf(warnings uint16, status uint16) error

	//send OK packet to the client
	sendOKPacket(affectedRows uint64, lastInsertId uint64, status uint16, warnings uint16, message string) error

	//the OK or EOF packet thread safe
	sendEOFOrOkPacket(warnings uint16, status uint16) error

	PrepareBeforeProcessingResultSet()

	GetStats() string

	ParseExecuteData(ctx context.Context, stmt *PrepareStmt, data []byte, pos int) (names []string, vars []any, err error)
}

var _ MysqlProtocol = &MysqlProtocolImpl{}

func (ses *Session) GetMysqlProtocol() MysqlProtocol {
	ses.mu.Lock()
	defer ses.mu.Unlock()
	return ses.protocol.(MysqlProtocol)
}

type debugStats struct {
	writeCount uint64
	writeBytes uint64
}

func (ds *debugStats) ResetStats() {
	ds.writeCount = 0
	ds.writeBytes = 0
}

func (ds *debugStats) String() string {
	if ds.writeCount <= 0 {
		ds.writeCount = 1
	}
	return fmt.Sprintf(
		"writeCount %v \n"+
			"writeBytes %v %v MB\n",
		ds.writeCount,
		ds.writeBytes, ds.writeBytes/(1024*1024.0),
	)
}

/*
rowHandler maintains the states in encoding the result row
*/
type rowHandler struct {
	//the begin position of writing.
	//the range [beginWriteIndex,beginWriteIndex+3]
	//for the length and sequenceId of the mysql protocol packet
	beginWriteIndex int
	//the bytes in the outbuffer
	bytesInOutBuffer int
	//when the number of bytes in the outbuffer exceeds the it,
	//the outbuffer will be flushed.
	untilBytesInOutbufToFlush int
	//the count of the flush
	flushCount int
	enableLog  bool
}

/*
isInPacket means it is compositing a packet now
*/
func (rh *rowHandler) isInPacket() bool {
	return rh.beginWriteIndex >= 0
}

/*
resetPacket reset the beginWriteIndex
*/
func (rh *rowHandler) resetPacket() {
	rh.beginWriteIndex = -1
}

/*
resetFlushOutBuffer clears the bytesInOutBuffer
*/
func (rh *rowHandler) resetFlushOutBuffer() {
	rh.bytesInOutBuffer = 0
}

/*
resetFlushCount reset flushCount
*/
func (rh *rowHandler) resetFlushCount() {
	rh.flushCount = 0
}

type MysqlProtocolImpl struct {
	ProtocolImpl

	//The sequence-id is incremented with each packet and may wrap around.
	//It starts at 0 and is reset to 0 when a new command begins in the Command Phase.
	sequenceId atomic.Uint32

	//joint capability shared by the server and the client
	capability uint32

	//collation id
	collationID int

	//collation name
	collationName string

	//character set
	charset string

	//max packet size of the client
	maxClientPacketSize uint32

	//the user of the client
	username string

	//the default database for the client
	database string

	//for debug
	debugStats

	//for converting the data into string
	strconvBuffer []byte

	//for encoding the length into bytes
	lenEncBuffer []byte

	//for encoding the null bytes in binary row
	binaryNullBuffer []byte

	rowHandler

	SV *config.FrontendParameters

	m sync.Mutex

	ses *Session

	//skip checking the password of the user
	skipCheckUser bool
}

func (mp *MysqlProtocolImpl) GetSession() *Session {
	mp.m.Lock()
	defer mp.m.Unlock()
	return mp.ses
}

func (mp *MysqlProtocolImpl) SetSkipCheckUser(b bool) {
	mp.m.Lock()
	defer mp.m.Unlock()
	mp.skipCheckUser = b
}

func (mp *MysqlProtocolImpl) GetCapability() uint32 {
	mp.m.Lock()
	defer mp.m.Unlock()
	return mp.capability
}

func (mp *MysqlProtocolImpl) GetSequenceId() uint8 {
	return uint8(mp.sequenceId.Load())
}

func (mp *MysqlProtocolImpl) AddSequenceId(a uint8) {
	mp.sequenceId.Add(uint32(a))
}

func (mp *MysqlProtocolImpl) GetSkipCheckUser() bool {
	mp.m.Lock()
	defer mp.m.Unlock()
	return mp.skipCheckUser
}

func (mp *MysqlProtocolImpl) GetDatabaseName() string {
	mp.m.Lock()
	defer mp.m.Unlock()
	return mp.database
}

func (mp *MysqlProtocolImpl) SetDatabaseName(s string) {
	mp.m.Lock()
	defer mp.m.Unlock()
	mp.database = s
}

func (mp *MysqlProtocolImpl) GetUserName() string {
	mp.m.Lock()
	defer mp.m.Unlock()
	return mp.username
}

func (mp *MysqlProtocolImpl) SetUserName(s string) {
	mp.m.Lock()
	defer mp.m.Unlock()
	mp.username = s
}

func (mp *MysqlProtocolImpl) GetStats() string {
	return fmt.Sprintf("flushCount %d %s",
		mp.flushCount,
		mp.String())
}

func (mp *MysqlProtocolImpl) PrepareBeforeProcessingResultSet() {
	mp.ResetStats()
	mp.resetFlushCount()
}

func (mp *MysqlProtocolImpl) Quit() {
	mp.ProtocolImpl.Quit()
}

func (mp *MysqlProtocolImpl) SetSession(ses *Session) {
	mp.m.Lock()
	defer mp.m.Unlock()
	mp.ses = ses
}

// handshake response 41
type response41 struct {
	capabilities      uint32
	maxPacketSize     uint32
	collationID       uint8
	username          string
	authResponse      []byte
	database          string
	clientPluginName  string
	isAskForTlsHeader bool
}

// handshake response 320
type response320 struct {
	capabilities      uint32
	maxPacketSize     uint32
	username          string
	authResponse      []byte
	database          string
	isAskForTlsHeader bool
}

func (mp *MysqlProtocolImpl) SendPrepareResponse(ctx context.Context, stmt *PrepareStmt) error {
	dcPrepare, ok := stmt.PreparePlan.GetDcl().Control.(*planPb.DataControl_Prepare)
	if !ok {
		return moerr.NewInternalError(ctx, "can not get Prepare plan in prepareStmt")
	}
	stmtID, err := GetPrepareStmtID(ctx, stmt.Name)
	if err != nil {
		return moerr.NewInternalError(ctx, "can not get Prepare stmtID")
	}
	paramTypes := dcPrepare.Prepare.ParamTypes
	numParams := len(paramTypes)
	columns := plan2.GetResultColumnsFromPlan(dcPrepare.Prepare.Plan)
	numColumns := len(columns)

	var data []byte
	// status ok
	data = append(data, 0)
	// stmt id
	data = mp.io.AppendUint32(data, uint32(stmtID))
	// number columns
	data = mp.io.AppendUint16(data, uint16(numColumns))
	// number params
	data = mp.io.AppendUint16(data, uint16(numParams))
	// filter [00]
	data = append(data, 0)
	// warning count
	data = append(data, 0, 0) // TODO support warning count
	if err := mp.writePackets(data); err != nil {
		return err
	}

	cmd := int(COM_STMT_PREPARE)
	for i := 0; i < numParams; i++ {
		column := new(MysqlColumn)
		column.SetName("?")

		err = convertEngineTypeToMysqlType(ctx, types.T(paramTypes[i]), column)
		if err != nil {
			return err
		}

		err = mp.SendColumnDefinitionPacket(ctx, column, cmd)
		if err != nil {
			return err
		}
	}
	if numParams > 0 {
		if err := mp.SendEOFPacketIf(0, 0); err != nil {
			return err
		}
	}

	for i := 0; i < numColumns; i++ {
		column := new(MysqlColumn)
		column.SetName(columns[i].Name)

		err = convertEngineTypeToMysqlType(ctx, types.T(columns[i].Typ.Id), column)
		if err != nil {
			return err
		}

		err = mp.SendColumnDefinitionPacket(ctx, column, cmd)
		if err != nil {
			return err
		}
	}
	if numColumns > 0 {
		if err := mp.SendEOFPacketIf(0, 0); err != nil {
			return err
		}
	}

	return nil
}

func (mp *MysqlProtocolImpl) ParseExecuteData(requestCtx context.Context, stmt *PrepareStmt, data []byte, pos int) (names []string, vars []any, err error) {
	dcPrepare, ok := stmt.PreparePlan.GetDcl().Control.(*planPb.DataControl_Prepare)
	if !ok {
		err = moerr.NewInternalError(requestCtx, "can not get Prepare plan in prepareStmt")
		return
	}
	numParams := len(dcPrepare.Prepare.ParamTypes)

	var flag uint8
	flag, pos, ok = mp.io.ReadUint8(data, pos)
	if !ok {
		err = moerr.NewInternalError(requestCtx, "malform packet")
		return
	}
	if flag != 0 {
		// TODO only support CURSOR_TYPE_NO_CURSOR flag now
		err = moerr.NewInvalidInput(requestCtx, "unsupported Prepare flag '%v'", flag)
		return
	}

	// skip iteration-count, always 1
	pos += 4

	if numParams > 0 {
		var nullBitmaps []byte
		nullBitmapLen := (numParams + 7) >> 3
		nullBitmaps, pos, ok = mp.readCountOfBytes(data, pos, nullBitmapLen)
		if !ok {
			err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
			return
		}

		// new param bound flag
		if data[pos] == 1 {
			pos++

			// Just the first StmtExecute packet contain parameters type,
			// we need save it for further use.
			stmt.ParamTypes, pos, ok = mp.readCountOfBytes(data, pos, numParams<<1)
			if !ok {
				err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
				return
			}
		} else {
			pos++
		}

		// get paramters and set value to session variables
		names = make([]string, numParams)
		vars = make([]any, numParams)
		for i := 0; i < numParams; i++ {
			varName := getPrepareStmtSessionVarName(i)
			names[i] = varName

			// TODO :if params had received via COM_STMT_SEND_LONG_DATA, use them directly.
			// ref https://dev.mysql.com/doc/internals/en/com-stmt-send-long-data.html

			if nullBitmaps[i>>3]&(1<<(uint(i)%8)) > 0 {
				vars[i] = nil
				continue
			}

			if (i<<1)+1 >= len(stmt.ParamTypes) {
				err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
				return
			}
			tp := stmt.ParamTypes[i<<1]
			isUnsigned := (stmt.ParamTypes[(i<<1)+1] & 0x80) > 0

			switch defines.MysqlType(tp) {
			case defines.MYSQL_TYPE_NULL:
				vars[i] = nil

			case defines.MYSQL_TYPE_TINY:
				val, newPos, ok := mp.io.ReadUint8(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}

				pos = newPos
				if isUnsigned {
					vars[i] = val
				} else {
					vars[i] = int8(val)
				}

			case defines.MYSQL_TYPE_SHORT, defines.MYSQL_TYPE_YEAR:
				val, newPos, ok := mp.io.ReadUint16(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}

				pos = newPos
				if isUnsigned {
					vars[i] = val
				} else {
					vars[i] = int16(val)
				}

			case defines.MYSQL_TYPE_INT24, defines.MYSQL_TYPE_LONG:
				val, newPos, ok := mp.io.ReadUint32(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}

				pos = newPos
				if isUnsigned {
					vars[i] = val
				} else {
					vars[i] = int32(val)
				}

			case defines.MYSQL_TYPE_LONGLONG:
				val, newPos, ok := mp.io.ReadUint64(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}

				pos = newPos
				if isUnsigned {
					vars[i] = val
				} else {
					vars[i] = int64(val)
				}

			case defines.MYSQL_TYPE_FLOAT:
				val, newPos, ok := mp.io.ReadUint32(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
				pos = newPos
				vars[i] = math.Float32frombits(val)

			case defines.MYSQL_TYPE_DOUBLE:
				val, newPos, ok := mp.io.ReadUint64(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
				pos = newPos
				vars[i] = math.Float64frombits(val)

			case defines.MYSQL_TYPE_VARCHAR, defines.MYSQL_TYPE_VAR_STRING, defines.MYSQL_TYPE_STRING, defines.MYSQL_TYPE_DECIMAL,
				defines.MYSQL_TYPE_ENUM, defines.MYSQL_TYPE_SET, defines.MYSQL_TYPE_GEOMETRY, defines.MYSQL_TYPE_BIT:
				val, newPos, ok := mp.readStringLenEnc(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
				pos = newPos
				vars[i] = val

			case defines.MYSQL_TYPE_BLOB, defines.MYSQL_TYPE_TINY_BLOB, defines.MYSQL_TYPE_MEDIUM_BLOB, defines.MYSQL_TYPE_LONG_BLOB, defines.MYSQL_TYPE_TEXT:
				val, newPos, ok := mp.readStringLenEnc(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
				pos = newPos
				vars[i] = []byte(val)

			case defines.MYSQL_TYPE_TIME:
				// See https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_binary_resultset.html
				// for more details.
				length, newPos, ok := mp.io.ReadUint8(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
				pos = newPos
				switch length {
				case 0:
					vars[i] = "0d 00:00:00"
				case 8, 12:
					pos, vars[i] = mp.readTime(data, pos, length)
				default:
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
			case defines.MYSQL_TYPE_DATE, defines.MYSQL_TYPE_DATETIME, defines.MYSQL_TYPE_TIMESTAMP:
				// See https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_binary_resultset.html
				// for more details.
				length, newPos, ok := mp.io.ReadUint8(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
				pos = newPos
				switch length {
				case 0:
					vars[i] = "0000-00-00 00:00:00"
				case 4:
					pos, vars[i] = mp.readDate(data, pos)
				case 7:
					pos, vars[i] = mp.readDateTime(data, pos)
				case 11:
					pos, vars[i] = mp.readTimestamp(data, pos)
				default:
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}

			case defines.MYSQL_TYPE_NEWDECIMAL:
				// use string for decimal.  Not tested
				val, newPos, ok := mp.readStringLenEnc(data, pos)
				if !ok {
					err = moerr.NewInvalidInput(requestCtx, "mysql protocol error, malformed packet")
					return
				}
				pos = newPos
				vars[i] = val

			default:
				err = moerr.NewInternalError(requestCtx, "unsupport parameter type")
				return
			}
		}
	}

	return
}

func (mp *MysqlProtocolImpl) readDate(data []byte, pos int) (int, string) {
	year, pos, _ := mp.io.ReadUint16(data, pos)
	month := data[pos]
	pos++
	day := data[pos]
	pos++
	return pos, fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

func (mp *MysqlProtocolImpl) readTime(data []byte, pos int, len uint8) (int, string) {
	var symbol byte
	negate := data[pos]
	pos++
	if negate == 1 {
		symbol = '-'
	}
	day, pos, _ := mp.io.ReadUint32(data, pos)
	hour := data[pos]
	pos++
	minute := data[pos]
	pos++
	second := data[pos]
	pos++
	// time with ms
	if len == 12 {
		ms, pos, _ := mp.io.ReadUint32(data, pos)
		if day > 0 {
			return pos, fmt.Sprintf("%c%dd %02d:%02d:%02d.%06d", symbol, day, hour, minute, second, ms)
		} else {
			return pos, fmt.Sprintf("%c%02d:%02d:%02d.%06d", symbol, hour, minute, second, ms)
		}
	}

	if day > 0 {
		return pos, fmt.Sprintf("%c%dd %02d:%02d:%02d", symbol, day, hour, minute, second)
	} else {
		return pos, fmt.Sprintf("%c%02d:%02d:%02d", symbol, hour, minute, second)
	}
}

func (mp *MysqlProtocolImpl) readDateTime(data []byte, pos int) (int, string) {
	pos, date := mp.readDate(data, pos)
	hour := data[pos]
	pos++
	minute := data[pos]
	pos++
	second := data[pos]
	pos++
	return pos, fmt.Sprintf("%s %02d:%02d:%02d", date, hour, minute, second)
}

func (mp *MysqlProtocolImpl) readTimestamp(data []byte, pos int) (int, string) {
	pos, dateTime := mp.readDateTime(data, pos)
	microSecond, pos, _ := mp.io.ReadUint32(data, pos)
	return pos, fmt.Sprintf("%s.%06d", dateTime, microSecond)
}

// read an int with length encoded from the buffer at the position
// return the int ; position + the count of bytes for length encoded (1 or 3 or 4 or 9)
func (mp *MysqlProtocolImpl) readIntLenEnc(data []byte, pos int) (uint64, int, bool) {
	if pos >= len(data) {
		return 0, 0, false
	}
	switch data[pos] {
	case 0xfb:
		//zero, one byte
		return 0, pos + 1, true
	case 0xfc:
		// int in two bytes
		if pos+2 >= len(data) {
			return 0, 0, false
		}
		value := uint64(data[pos+1]) |
			uint64(data[pos+2])<<8
		return value, pos + 3, true
	case 0xfd:
		// int in three bytes
		if pos+3 >= len(data) {
			return 0, 0, false
		}
		value := uint64(data[pos+1]) |
			uint64(data[pos+2])<<8 |
			uint64(data[pos+3])<<16
		return value, pos + 4, true
	case 0xfe:
		// int in eight bytes
		if pos+8 >= len(data) {
			return 0, 0, false
		}
		value := uint64(data[pos+1]) |
			uint64(data[pos+2])<<8 |
			uint64(data[pos+3])<<16 |
			uint64(data[pos+4])<<24 |
			uint64(data[pos+5])<<32 |
			uint64(data[pos+6])<<40 |
			uint64(data[pos+7])<<48 |
			uint64(data[pos+8])<<56
		return value, pos + 9, true
	}
	// 0-250
	return uint64(data[pos]), pos + 1, true
}

// write an int with length encoded into the buffer at the position
// return position + the count of bytes for length encoded (1 or 3 or 4 or 9)
func (mp *MysqlProtocolImpl) writeIntLenEnc(data []byte, pos int, value uint64) int {
	switch {
	case value < 251:
		data[pos] = byte(value)
		return pos + 1
	case value < (1 << 16):
		data[pos] = 0xfc
		data[pos+1] = byte(value)
		data[pos+2] = byte(value >> 8)
		return pos + 3
	case value < (1 << 24):
		data[pos] = 0xfd
		data[pos+1] = byte(value)
		data[pos+2] = byte(value >> 8)
		data[pos+3] = byte(value >> 16)
		return pos + 4
	default:
		data[pos] = 0xfe
		data[pos+1] = byte(value)
		data[pos+2] = byte(value >> 8)
		data[pos+3] = byte(value >> 16)
		data[pos+4] = byte(value >> 24)
		data[pos+5] = byte(value >> 32)
		data[pos+6] = byte(value >> 40)
		data[pos+7] = byte(value >> 48)
		data[pos+8] = byte(value >> 56)
		return pos + 9
	}
}

// append an int with length encoded to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendIntLenEnc(data []byte, value uint64) []byte {
	mp.lenEncBuffer = mp.lenEncBuffer[:9]
	pos := mp.writeIntLenEnc(mp.lenEncBuffer, 0, value)
	return mp.append(data, mp.lenEncBuffer[:pos]...)
}

// read the count of bytes from the buffer at the position
// return bytes slice ; position + count ; true - succeeded or false - failed
func (mp *MysqlProtocolImpl) readCountOfBytes(data []byte, pos int, count int) ([]byte, int, bool) {
	if pos+count-1 >= len(data) {
		return nil, 0, false
	}
	return data[pos : pos+count], pos + count, true
}

// write the count of bytes into the buffer at the position
// return position + the number of bytes
func (mp *MysqlProtocolImpl) writeCountOfBytes(data []byte, pos int, value []byte) int {
	pos += copy(data[pos:], value)
	return pos
}

// append the count of bytes to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendCountOfBytes(data []byte, value []byte) []byte {
	return mp.append(data, value...)
}

// read a string with fixed length from the buffer at the position
// return string ; position + length ; true - succeeded or false - failed
func (mp *MysqlProtocolImpl) readStringFix(data []byte, pos int, length int) (string, int, bool) {
	var sdata []byte
	var ok bool
	sdata, pos, ok = mp.readCountOfBytes(data, pos, length)
	if !ok {
		return "", 0, false
	}
	return string(sdata), pos, true
}

// write a string with fixed length into the buffer at the position
// return pos + string.length
func (mp *MysqlProtocolImpl) writeStringFix(data []byte, pos int, value string, length int) int {
	pos += copy(data[pos:], value[0:length])
	return pos
}

// append a string with fixed length to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendStringFix(data []byte, value string, length int) []byte {
	return mp.append(data, []byte(value[:length])...)
}

// read a string appended with zero from the buffer at the position
// return string ; position + length of the string + 1; true - succeeded or false - failed
func (mp *MysqlProtocolImpl) readStringNUL(data []byte, pos int) (string, int, bool) {
	zeroPos := bytes.IndexByte(data[pos:], 0)
	if zeroPos == -1 {
		return "", 0, false
	}
	return string(data[pos : pos+zeroPos]), pos + zeroPos + 1, true
}

// write a string into the buffer at the position, then appended with 0
// return pos + string.length + 1
func (mp *MysqlProtocolImpl) writeStringNUL(data []byte, pos int, value string) int {
	pos = mp.writeStringFix(data, pos, value, len(value))
	data[pos] = 0
	return pos + 1
}

// read a string with length encoded from the buffer at the position
// return string ; position + the count of bytes for length encoded (1 or 3 or 4 or 9) + length of the string; true - succeeded or false - failed
func (mp *MysqlProtocolImpl) readStringLenEnc(data []byte, pos int) (string, int, bool) {
	var value uint64
	var ok bool
	value, pos, ok = mp.readIntLenEnc(data, pos)
	if !ok {
		return "", 0, false
	}
	sLength := int(value)
	if pos+sLength-1 >= len(data) {
		return "", 0, false
	}
	return string(data[pos : pos+sLength]), pos + sLength, true
}

// write a string with length encoded into the buffer at the position
// return position + the count of bytes for length encoded (1 or 3 or 4 or 9) + length of the string;
func (mp *MysqlProtocolImpl) writeStringLenEnc(data []byte, pos int, value string) int {
	pos = mp.writeIntLenEnc(data, pos, uint64(len(value)))
	return mp.writeStringFix(data, pos, value, len(value))
}

// append a string with length encoded to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendStringLenEnc(data []byte, value string) []byte {
	data = mp.appendIntLenEnc(data, uint64(len(value)))
	return mp.appendStringFix(data, value, len(value))
}

// append bytes with length encoded to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendCountOfBytesLenEnc(data []byte, value []byte) []byte {
	data = mp.appendIntLenEnc(data, uint64(len(value)))
	return mp.appendCountOfBytes(data, value)
}

// append an int64 value converted to string with length encoded to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendStringLenEncOfInt64(data []byte, value int64) []byte {
	mp.strconvBuffer = mp.strconvBuffer[:0]
	mp.strconvBuffer = strconv.AppendInt(mp.strconvBuffer, value, 10)
	return mp.appendCountOfBytesLenEnc(data, mp.strconvBuffer)
}

// append an uint64 value converted to string with length encoded to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendStringLenEncOfUint64(data []byte, value uint64) []byte {
	mp.strconvBuffer = mp.strconvBuffer[:0]
	mp.strconvBuffer = strconv.AppendUint(mp.strconvBuffer, value, 10)
	return mp.appendCountOfBytesLenEnc(data, mp.strconvBuffer)
}

// append an float32 value converted to string with length encoded to the buffer
// return the buffer
func (mp *MysqlProtocolImpl) appendStringLenEncOfFloat64(data []byte, value float64, bitSize int) []byte {
	mp.strconvBuffer = mp.strconvBuffer[:0]
	if !math.IsInf(value, 0) {
		mp.strconvBuffer = strconv.AppendFloat(mp.strconvBuffer, value, 'f', -1, bitSize)
	} else {
		if math.IsInf(value, 1) {
			mp.strconvBuffer = append(mp.strconvBuffer, []byte("+Infinity")...)
		} else {
			mp.strconvBuffer = append(mp.strconvBuffer, []byte("-Infinity")...)
		}
	}
	return mp.appendCountOfBytesLenEnc(data, mp.strconvBuffer)
}

func (mp *MysqlProtocolImpl) appendUint8(data []byte, e uint8) []byte {
	return mp.append(data, e)
}

func (mp *MysqlProtocolImpl) appendUint16(data []byte, e uint16) []byte {
	buf := mp.lenEncBuffer[:2]
	pos := mp.io.WriteUint16(buf, 0, e)
	return mp.append(data, buf[:pos]...)
}

func (mp *MysqlProtocolImpl) appendUint32(data []byte, e uint32) []byte {
	buf := mp.lenEncBuffer[:4]
	pos := mp.io.WriteUint32(buf, 0, e)
	return mp.append(data, buf[:pos]...)
}

func (mp *MysqlProtocolImpl) appendUint64(data []byte, e uint64) []byte {
	buf := mp.lenEncBuffer[:8]
	pos := mp.io.WriteUint64(buf, 0, e)
	return mp.append(data, buf[:pos]...)
}

// write the count of zeros into the buffer at the position
// return pos + count
func (mp *MysqlProtocolImpl) writeZeros(data []byte, pos int, count int) int {
	for i := 0; i < count; i++ {
		data[pos+i] = 0
	}
	return pos + count
}

// the server calculates the hash value of the password with the algorithm
// and judges it with the authentication data from the client.
// Algorithm: SHA1( password ) XOR SHA1( slat + SHA1( SHA1( password ) ) )
func (mp *MysqlProtocolImpl) checkPassword(password, salt, auth []byte) bool {
	//if len(password) == 0 {
	//	return false
	//}
	//hash1 = SHA1(password)
	sha := sha1.New()
	_, err := sha.Write(password)
	if err != nil {
		logutil.Errorf("SHA1(password) failed.")
		return false
	}
	hash1 := sha.Sum(nil)

	//hash2 = SHA1(SHA1(password))
	sha.Reset()
	_, err = sha.Write(hash1)
	if err != nil {
		logutil.Errorf("SHA1(SHA1(password)) failed.")
		return false
	}
	hash2 := sha.Sum(nil)

	//hash3 = SHA1(salt + SHA1(SHA1(password)))
	sha.Reset()
	_, err = sha.Write(salt)
	if err != nil {
		logutil.Errorf("write salt failed.")
		return false
	}
	_, err = sha.Write(hash2)
	if err != nil {
		logutil.Errorf("write SHA1(SHA1(password)) failed.")
		return false
	}
	hash3 := sha.Sum(nil)

	//SHA1(password) XOR SHA1(salt + SHA1(SHA1(password)))
	for i := range hash1 {
		hash1[i] ^= hash3[i]
	}

	logDebugf(mp.getProfile(profileTypeConcise), "server calculated %v", hash1)
	logDebugf(mp.getProfile(profileTypeConcise), "client calculated %v", auth)

	return bytes.Equal(hash1, auth)
}

// the server authenticate that the client can connect and use the database
func (mp *MysqlProtocolImpl) authenticateUser(ctx context.Context, authResponse []byte) error {
	var psw []byte
	var err error
	var tenant *TenantInfo

	ses := mp.GetSession()
	if !mp.GetSkipCheckUser() {
		logDebugf(mp.getProfile(profileTypeConcise), "authenticate user 1")
		psw, err = ses.AuthenticateUser(mp.GetUserName())
		if err != nil {
			return err
		}
		logDebugf(mp.getProfile(profileTypeConcise), "authenticate user 2")

		//TO Check password
		if mp.checkPassword(psw, mp.GetSalt(), authResponse) {
			logInfof(mp.getProfile(profileTypeConcise), "check password succeeded")
		} else {
			return moerr.NewInternalError(ctx, "check password failed")
		}
	} else {
		logDebugf(mp.getProfile(profileTypeConcise), "skip authenticate user")
		//Get tenant info
		tenant, err = GetTenantInfo(ctx, mp.GetUserName())
		if err != nil {
			return err
		}

		if ses != nil {
			ses.SetTenantInfo(tenant)

			//TO Check password
			if len(psw) == 0 || mp.checkPassword(psw, mp.GetSalt(), authResponse) {
				logInfof(mp.getProfile(profileTypeConcise), "check password succeeded")
			} else {
				return moerr.NewInternalError(ctx, "check password failed")
			}
		}
	}

	return nil
}

func (mp *MysqlProtocolImpl) SetSequenceID(value uint8) {
	mp.sequenceId.Store(uint32(value))
}

func (mp *MysqlProtocolImpl) handleHandshake(ctx context.Context, payload []byte) (bool, error) {
	var err, err2 error
	if len(payload) < 2 {
		return false, moerr.NewInternalError(ctx, "received a broken response packet")
	}

	var authResponse []byte
	if capabilities, _, ok := mp.io.ReadUint16(payload, 0); !ok {
		return false, moerr.NewInternalError(ctx, "read capabilities from response packet failed")
	} else if uint32(capabilities)&CLIENT_PROTOCOL_41 != 0 {
		var resp41 response41
		var ok2 bool
		logDebugf(mp.getProfile(profileTypeConcise), "analyse handshake response")
		if ok2, resp41, err = mp.analyseHandshakeResponse41(ctx, payload); !ok2 {
			return false, err
		}

		// client ask server to upgradeTls
		if resp41.isAskForTlsHeader {
			return true, nil
		}

		authResponse = resp41.authResponse
		mp.capability = mp.capability & resp41.capabilities

		if nameAndCharset, ok3 := collationID2CharsetAndName[int(resp41.collationID)]; !ok3 {
			return false, moerr.NewInternalError(ctx, "get collationName and charset failed")
		} else {
			mp.collationID = int(resp41.collationID)
			mp.collationName = nameAndCharset.collationName
			mp.charset = nameAndCharset.charset
		}

		mp.maxClientPacketSize = resp41.maxPacketSize
		mp.username = resp41.username
		mp.database = resp41.database
	} else {
		var resp320 response320
		var ok2 bool
		if ok2, resp320, err = mp.analyseHandshakeResponse320(ctx, payload); !ok2 {
			return false, err
		}

		// client ask server to upgradeTls
		if resp320.isAskForTlsHeader {
			return true, nil
		}

		authResponse = resp320.authResponse
		mp.capability = mp.capability & resp320.capabilities
		mp.collationID = int(Utf8mb4CollationID)
		mp.collationName = "utf8mb4_general_ci"
		mp.charset = "utf8mb4"

		mp.maxClientPacketSize = resp320.maxPacketSize
		mp.username = resp320.username
		mp.database = resp320.database
	}

	logDebugf(mp.getProfile(profileTypeConcise), "authenticate user")
	if err = mp.authenticateUser(ctx, authResponse); err != nil {
		logutil.Errorf("authenticate user failed.error:%v", err)
		fail := moerr.MysqlErrorMsgRefer[moerr.ER_ACCESS_DENIED_ERROR]
		tipsFormat := "Access denied for user %s. %s"
		msg := fmt.Sprintf(tipsFormat, mp.username, err.Error())
		err2 = mp.sendErrPacket(fail.ErrorCode, fail.SqlStates[0], msg)
		if err2 != nil {
			logutil.Errorf("send err packet failed.error:%v", err2)
			return false, err2
		}
		return false, err
	}

	logDebugf(mp.getProfile(profileTypeConcise), "handle handshake end")
	err = mp.sendOKPacket(0, 0, 0, 0, "")
	if err != nil {
		return false, err
	}
	return false, nil
}

// the server makes a handshake v10 packet
// return handshake packet
func (mp *MysqlProtocolImpl) makeHandshakeV10Payload() []byte {
	var data = make([]byte, HeaderOffset+256)
	var pos = HeaderOffset
	//int<1> protocol version
	pos = mp.io.WriteUint8(data, pos, clientProtocolVersion)

	pos = mp.writeStringNUL(data, pos, mp.SV.ServerVersionPrefix+serverVersion.Load().(string))

	//int<4> connection id
	pos = mp.io.WriteUint32(data, pos, mp.ConnectionID())

	//string[8] auth-plugin-data-part-1
	pos = mp.writeCountOfBytes(data, pos, mp.GetSalt()[0:8])

	//int<1> filler 0
	pos = mp.io.WriteUint8(data, pos, 0)

	//int<2>              capabilities flags (lower 2 bytes)
	pos = mp.io.WriteUint16(data, pos, uint16(mp.capability&0xFFFF))

	//int<1>              character set
	pos = mp.io.WriteUint8(data, pos, utf8mb4BinCollationID)

	//int<2>              status flags
	pos = mp.io.WriteUint16(data, pos, DefaultClientConnStatus)

	//int<2>              capabilities flags (upper 2 bytes)
	pos = mp.io.WriteUint16(data, pos, uint16((DefaultCapability>>16)&0xFFFF))

	if (DefaultCapability & CLIENT_PLUGIN_AUTH) != 0 {
		//int<1>              length of auth-plugin-data
		//set 21 always
		pos = mp.io.WriteUint8(data, pos, uint8(len(mp.GetSalt())+1))
	} else {
		//int<1>              [00]
		//set 0 always
		pos = mp.io.WriteUint8(data, pos, 0)
	}

	//string[10]     reserved (all [00])
	pos = mp.writeZeros(data, pos, 10)

	if (DefaultCapability & CLIENT_SECURE_CONNECTION) != 0 {
		//string[$len]   auth-plugin-data-part-2 ($len=MAX(13, length of auth-plugin-data - 8))
		pos = mp.writeCountOfBytes(data, pos, mp.GetSalt()[8:])
		pos = mp.io.WriteUint8(data, pos, 0)
	}

	if (DefaultCapability & CLIENT_PLUGIN_AUTH) != 0 {
		//string[NUL]    auth-plugin name
		pos = mp.writeStringNUL(data, pos, AuthNativePassword)
	}

	return data[:pos]
}

// the server analyses handshake response41 info from the client
// return true - analysed successfully / false - failed ; response41 ; error
func (mp *MysqlProtocolImpl) analyseHandshakeResponse41(ctx context.Context, data []byte) (bool, response41, error) {
	var pos = 0
	var ok bool
	var info response41

	//int<4>             capabilities flags of the client, CLIENT_PROTOCOL_41 always set
	info.capabilities, pos, ok = mp.io.ReadUint32(data, pos)
	if !ok {
		return false, info, moerr.NewInternalError(ctx, "get capabilities failed")
	}

	if (info.capabilities & CLIENT_PROTOCOL_41) == 0 {
		return false, info, moerr.NewInternalError(ctx, "capabilities does not have protocol 41")
	}

	//int<4>             max-packet size
	//max size of a command packet that the client wants to send to the server
	info.maxPacketSize, pos, ok = mp.io.ReadUint32(data, pos)
	if !ok {
		return false, info, moerr.NewInternalError(ctx, "get max packet size failed")
	}

	//int<1>             character set
	//connection's default character set
	info.collationID, pos, ok = mp.io.ReadUint8(data, pos)
	if !ok {
		return false, info, moerr.NewInternalError(ctx, "get character set failed")
	}

	if pos+22 >= len(data) {
		return false, info, moerr.NewInternalError(ctx, "skip reserved failed")
	}
	//string[23]         reserved (all [0])
	//just skip it
	pos += 23

	// if client reply for upgradeTls, then data will contains header only.
	if pos == len(data) && (info.capabilities&CLIENT_SSL) != 0 {
		info.isAskForTlsHeader = true
		return true, info, nil
	}

	//string[NUL]        username
	info.username, pos, ok = mp.readStringNUL(data, pos)
	if !ok {
		return false, info, moerr.NewInternalError(ctx, "get username failed")
	}

	/*
		if capabilities & CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA {
			lenenc-int         length of auth-response
			string[n]          auth-response
		} else if capabilities & CLIENT_SECURE_CONNECTION {
			int<1>             length of auth-response
			string[n]           auth-response
		} else {
			string[NUL]        auth-response
		}
	*/
	if (info.capabilities & CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA) != 0 {
		var l uint64
		l, pos, ok = mp.readIntLenEnc(data, pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get length of auth-response failed")
		}
		info.authResponse, pos, ok = mp.readCountOfBytes(data, pos, int(l))
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get auth-response failed")
		}
	} else if (info.capabilities & CLIENT_SECURE_CONNECTION) != 0 {
		var l uint8
		l, pos, ok = mp.io.ReadUint8(data, pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get length of auth-response failed")
		}
		info.authResponse, pos, ok = mp.readCountOfBytes(data, pos, int(l))
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get auth-response failed")
		}
	} else {
		var auth string
		auth, pos, ok = mp.readStringNUL(data, pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get auth-response failed")
		}
		info.authResponse = []byte(auth)
	}

	if (info.capabilities & CLIENT_CONNECT_WITH_DB) != 0 {
		info.database, pos, ok = mp.readStringNUL(data, pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get database failed")
		}
	}

	if (info.capabilities & CLIENT_PLUGIN_AUTH) != 0 {
		info.clientPluginName, _, ok = mp.readStringNUL(data, pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get auth plugin name failed")
		}

		//to switch authenticate method
		if info.clientPluginName != AuthNativePassword {
			var err error
			if info.authResponse, err = mp.negotiateAuthenticationMethod(ctx); err != nil {
				return false, info, moerr.NewInternalError(ctx, "negotiate authentication method failed. error:%v", err)
			}
			info.clientPluginName = AuthNativePassword
		}
	}

	//drop client connection attributes
	return true, info, nil
}

/*
//the server does something after receiving a handshake response41 from the client
//like check user and password
//and other things
func (mp *MysqlProtocolImpl) handleClientResponse41(resp41 response41) error {
	//to do something else
	//logutil.Infof("capabilities 0x%x\n", resp41.capabilities)
	//logutil.Infof("maxPacketSize %d\n", resp41.maxPacketSize)
	//logutil.Infof("collationID %d\n", resp41.collationID)
	//logutil.Infof("username %s\n", resp41.username)
	//logutil.Infof("authResponse: \n")
	//update the capabilities with client's capabilities
	mp.capability = DefaultCapability & resp41.capabilities

	//character set
	if nameAndCharset, ok := collationID2CharsetAndName[int(resp41.collationID)]; !ok {
		return moerr.NewInternalError(requestCtx, "get collationName and charset failed")
	} else {
		mp.collationID = int(resp41.collationID)
		mp.collationName = nameAndCharset.collationName
		mp.charset = nameAndCharset.charset
	}

	mp.maxClientPacketSize = resp41.maxPacketSize
	mp.username = resp41.username
	mp.database = resp41.database

	//logutil.Infof("collationID %d collatonName %s charset %s \n", mp.collationID, mp.collationName, mp.charset)
	//logutil.Infof("database %s \n", resp41.database)
	//logutil.Infof("clientPluginName %s \n", resp41.clientPluginName)
	return nil
}
*/

// the server analyses handshake response320 info from the old client
// return true - analysed successfully / false - failed ; response320 ; error
func (mp *MysqlProtocolImpl) analyseHandshakeResponse320(ctx context.Context, data []byte) (bool, response320, error) {
	var pos = 0
	var ok bool
	var info response320
	var capa uint16

	//int<2>             capabilities flags, CLIENT_PROTOCOL_41 never set
	capa, pos, ok = mp.io.ReadUint16(data, pos)
	if !ok {
		return false, info, moerr.NewInternalError(ctx, "get capabilities failed")
	}
	info.capabilities = uint32(capa)

	if pos+2 >= len(data) {
		return false, info, moerr.NewInternalError(ctx, "get max-packet-size failed")
	}

	//int<3>             max-packet size
	//max size of a command packet that the client wants to send to the server
	info.maxPacketSize = uint32(data[pos]) | uint32(data[pos+1])<<8 | uint32(data[pos+2])<<16
	pos += 3

	// if client reply for upgradeTls, then data will contains header only.
	if pos == len(data) && (info.capabilities&CLIENT_SSL) != 0 {
		info.isAskForTlsHeader = true
		return true, info, nil
	}

	//string[NUL]        username
	info.username, pos, ok = mp.readStringNUL(data, pos)
	if !ok {
		return false, info, moerr.NewInternalError(ctx, "get username failed")
	}

	if (info.capabilities & CLIENT_CONNECT_WITH_DB) != 0 {
		var auth string
		auth, pos, ok = mp.readStringNUL(data, pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get auth-response failed")
		}
		info.authResponse = []byte(auth)

		info.database, _, ok = mp.readStringNUL(data, pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get database failed")
		}
	} else {
		info.authResponse, _, ok = mp.readCountOfBytes(data, pos, len(data)-pos)
		if !ok {
			return false, info, moerr.NewInternalError(ctx, "get auth-response failed")
		}
	}

	return true, info, nil
}

/*
//the server does something after receiving a handshake response320 from the client
//like check user and password
//and other things
func (mp *MysqlProtocolImpl) handleClientResponse320(resp320 response320) error {
	//to do something else
	//logutil.Infof("capabilities 0x%x\n", resp320.capabilities)
	//logutil.Infof("maxPacketSize %d\n", resp320.maxPacketSize)
	//logutil.Infof("username %s\n", resp320.username)
	//logutil.Infof("authResponse: \n")

	//update the capabilities with client's capabilities
	mp.capability = DefaultCapability & resp320.capabilities

	//if the client does not notice its default charset, the server gives a default charset.
	//Run the sql in mysql 8.0.23 to get the charset
	//the sql: select * from information_schema.collations where collation_name = 'utf8mb4_general_ci';
	mp.collationID = int(Utf8mb4CollationID)
	mp.collationName = "utf8mb4_general_ci"
	mp.charset = "utf8mb4"

	mp.maxClientPacketSize = resp320.maxPacketSize
	mp.username = resp320.username
	mp.database = resp320.database

	//logutil.Infof("collationID %d collatonName %s charset %s \n", mp.collationID, mp.collationName, mp.charset)
	//logutil.Infof("database %s \n", resp320.database)
	return nil
}
*/

// the server makes a AuthSwitchRequest that asks the client to authenticate the data with new method
func (mp *MysqlProtocolImpl) makeAuthSwitchRequestPayload(authMethodName string) []byte {
	data := make([]byte, HeaderOffset+1+len(authMethodName)+1+len(mp.GetSalt())+1)
	pos := HeaderOffset
	pos = mp.io.WriteUint8(data, pos, defines.EOFHeader)
	pos = mp.writeStringNUL(data, pos, authMethodName)
	pos = mp.writeCountOfBytes(data, pos, mp.GetSalt())
	pos = mp.io.WriteUint8(data, pos, 0)
	return data[:pos]
}

// the server can send AuthSwitchRequest to ask client to use designated authentication method,
// if both server and client support CLIENT_PLUGIN_AUTH capability.
// return data authenticated with new method
func (mp *MysqlProtocolImpl) negotiateAuthenticationMethod(ctx context.Context) ([]byte, error) {
	var err error
	aswPkt := mp.makeAuthSwitchRequestPayload(AuthNativePassword)
	err = mp.writePackets(aswPkt)
	if err != nil {
		return nil, err
	}

	read, err := mp.tcpConn.Read(goetty.ReadOptions{})
	if err != nil {
		return nil, err
	}

	if read == nil {
		return nil, moerr.NewInternalError(ctx, "read nil from tcp conn")
	}

	pack, ok := read.(*Packet)
	if !ok {
		return nil, moerr.NewInternalError(ctx, "it is not the Packet")
	}

	if pack == nil {
		return nil, moerr.NewInternalError(ctx, "packet is null")
	}

	data := pack.Payload
	mp.AddSequenceId(1)
	return data, nil
}

// make a OK packet
func (mp *MysqlProtocolImpl) makeOKPayload(affectedRows, lastInsertId uint64, statusFlags, warnings uint16, message string) []byte {
	data := make([]byte, HeaderOffset+128+len(message)+10)
	var pos = HeaderOffset
	pos = mp.io.WriteUint8(data, pos, defines.OKHeader)
	pos = mp.writeIntLenEnc(data, pos, affectedRows)
	pos = mp.writeIntLenEnc(data, pos, lastInsertId)
	if (mp.capability & CLIENT_PROTOCOL_41) != 0 {
		pos = mp.io.WriteUint16(data, pos, statusFlags)
		pos = mp.io.WriteUint16(data, pos, warnings)
	} else if (mp.capability & CLIENT_TRANSACTIONS) != 0 {
		pos = mp.io.WriteUint16(data, pos, statusFlags)
	}

	if mp.capability&CLIENT_SESSION_TRACK != 0 {
		//TODO:implement it
	} else {
		//string<lenenc> instead of string<EOF> in the manual of mysql
		pos = mp.writeStringLenEnc(data, pos, message)
		return data[:pos]
	}
	return data[:pos]
}

func (mp *MysqlProtocolImpl) makeOKPayloadWithEof(affectedRows, lastInsertId uint64, statusFlags, warnings uint16, message string) []byte {
	data := make([]byte, HeaderOffset+128+len(message)+10)
	var pos = HeaderOffset
	pos = mp.io.WriteUint8(data, pos, defines.EOFHeader)
	pos = mp.writeIntLenEnc(data, pos, affectedRows)
	pos = mp.writeIntLenEnc(data, pos, lastInsertId)
	if (mp.capability & CLIENT_PROTOCOL_41) != 0 {
		pos = mp.io.WriteUint16(data, pos, statusFlags)
		pos = mp.io.WriteUint16(data, pos, warnings)
	} else if (mp.capability & CLIENT_TRANSACTIONS) != 0 {
		pos = mp.io.WriteUint16(data, pos, statusFlags)
	}

	if mp.capability&CLIENT_SESSION_TRACK != 0 {
		//TODO:implement it
	} else {
		//string<lenenc> instead of string<EOF> in the manual of mysql
		pos = mp.writeStringLenEnc(data, pos, message)
		return data[:pos]
	}
	return data[:pos]
}

func (mp *MysqlProtocolImpl) sendOKPacketWithEof(affectedRows, lastInsertId uint64, status, warnings uint16, message string) error {
	okPkt := mp.makeOKPayloadWithEof(affectedRows, lastInsertId, status, warnings, message)
	return mp.writePackets(okPkt)
}

// send OK packet to the client
func (mp *MysqlProtocolImpl) sendOKPacket(affectedRows, lastInsertId uint64, status, warnings uint16, message string) error {
	okPkt := mp.makeOKPayload(affectedRows, lastInsertId, status, warnings, message)
	return mp.writePackets(okPkt)
}

// make Err packet
func (mp *MysqlProtocolImpl) makeErrPayload(errorCode uint16, sqlState, errorMessage string) []byte {
	data := make([]byte, HeaderOffset+9+len(errorMessage))
	pos := HeaderOffset
	pos = mp.io.WriteUint8(data, pos, defines.ErrHeader)
	pos = mp.io.WriteUint16(data, pos, errorCode)
	if mp.capability&CLIENT_PROTOCOL_41 != 0 {
		pos = mp.io.WriteUint8(data, pos, '#')
		if len(sqlState) < 5 {
			stuff := "      "
			sqlState += stuff[:5-len(sqlState)]
		}
		pos = mp.writeStringFix(data, pos, sqlState, 5)
	}
	pos = mp.writeStringFix(data, pos, errorMessage, len(errorMessage))
	return data[:pos]
}

/*
the server sends the Error packet

information from https://dev.mysql.com/doc/mysql-errors/8.0/en/server-error-reference.html
mysql version 8.0.23
usually it is in the directory /usr/local/include/mysql/mysqld_error.h

Error information includes several elements: an error code, SQLSTATE value, and message string.

	Error code: This value is numeric. It is MySQL-specific and is not portable to other database systems.
	SQLSTATE value: This value is a five-character string (for example, '42S02'). SQLSTATE values are taken from ANSI SQL and ODBC and are more standardized than the numeric error codes.
	Message string: This string provides a textual description of the error.
*/
func (mp *MysqlProtocolImpl) sendErrPacket(errorCode uint16, sqlState, errorMessage string) error {
	if mp.ses != nil {
		mp.ses.GetErrInfo().push(errorCode, errorMessage)
	}
	errPkt := mp.makeErrPayload(errorCode, sqlState, errorMessage)
	return mp.writePackets(errPkt)
}

func (mp *MysqlProtocolImpl) makeEOFPayload(warnings, status uint16) []byte {
	data := make([]byte, HeaderOffset+10)
	pos := HeaderOffset
	pos = mp.io.WriteUint8(data, pos, defines.EOFHeader)
	if mp.capability&CLIENT_PROTOCOL_41 != 0 {
		pos = mp.io.WriteUint16(data, pos, warnings)
		pos = mp.io.WriteUint16(data, pos, status)
	}
	return data[:pos]
}

func (mp *MysqlProtocolImpl) sendEOFPacket(warnings, status uint16) error {
	data := mp.makeEOFPayload(warnings, status)
	return mp.writePackets(data)
}

func (mp *MysqlProtocolImpl) SendEOFPacketIf(warnings, status uint16) error {
	//If the CLIENT_DEPRECATE_EOF client capabilities flag is not set, EOF_Packet
	if mp.capability&CLIENT_DEPRECATE_EOF == 0 {
		return mp.sendEOFPacket(warnings, status)
	}
	return nil
}

// the OK or EOF packet
// thread safe
func (mp *MysqlProtocolImpl) sendEOFOrOkPacket(warnings, status uint16) error {
	//If the CLIENT_DEPRECATE_EOF client capabilities flag is set, OK_Packet; else EOF_Packet.
	if mp.capability&CLIENT_DEPRECATE_EOF != 0 {
		return mp.sendOKPacketWithEof(0, 0, status, 0, "")
	} else {
		return mp.sendEOFPacket(warnings, status)
	}
}

func setColLength(column *MysqlColumn, width int32) {
	column.length = column.columnType.GetLength(width)
}

func setColFlag(column *MysqlColumn) {
	if column.auto_incr {
		column.flag |= uint16(defines.AUTO_INCREMENT_FLAG)
	}
}

func setCharacter(column *MysqlColumn) {
	switch column.columnType {
	// blob type should use 0x3f to show the binary data
	case defines.MYSQL_TYPE_VARCHAR, defines.MYSQL_TYPE_STRING, defines.MYSQL_TYPE_TEXT, defines.MYSQL_TYPE_BLOB:
		column.SetCharset(0x21)
	default:
		column.SetCharset(0x3f)
	}
}

// make the column information with the format of column definition41
func (mp *MysqlProtocolImpl) makeColumnDefinition41Payload(column *MysqlColumn, cmd int) []byte {
	space := HeaderOffset + 8*9 + //lenenc bytes of 8 fields
		21 + //fixed-length fields
		3 + // catalog "def"
		len(column.Schema()) +
		len(column.Table()) +
		len(column.OrgTable()) +
		len(column.Name()) +
		len(column.OrgName()) +
		len(column.DefaultValue()) +
		100 // for safe

	data := make([]byte, space)
	pos := HeaderOffset

	//lenenc_str     catalog(always "def")
	pos = mp.writeStringLenEnc(data, pos, "def")

	//lenenc_str     schema
	pos = mp.writeStringLenEnc(data, pos, column.Schema())

	//lenenc_str     table
	pos = mp.writeStringLenEnc(data, pos, column.Table())

	//lenenc_str     org_table
	pos = mp.writeStringLenEnc(data, pos, column.OrgTable())

	//lenenc_str     name
	pos = mp.writeStringLenEnc(data, pos, column.Name())

	//lenenc_str     org_name
	pos = mp.writeStringLenEnc(data, pos, column.OrgName())

	//lenenc_int     length of fixed-length fields [0c]
	pos = mp.io.WriteUint8(data, pos, 0x0c)

	//int<2>              character set
	pos = mp.io.WriteUint16(data, pos, column.Charset())

	//int<4>              column length
	pos = mp.io.WriteUint32(data, pos, column.Length())

	//int<1>              type
	pos = mp.io.WriteUint8(data, pos, uint8(column.ColumnType()))

	//int<2>              flags
	pos = mp.io.WriteUint16(data, pos, column.Flag())

	//int<1>              decimals
	pos = mp.io.WriteUint8(data, pos, column.Decimal())

	//int<2>              filler [00] [00]
	pos = mp.io.WriteUint16(data, pos, 0)

	if CommandType(cmd) == COM_FIELD_LIST {
		pos = mp.writeIntLenEnc(data, pos, uint64(len(column.DefaultValue())))
		pos = mp.writeCountOfBytes(data, pos, column.DefaultValue())
	}

	return data[:pos]
}

// SendColumnDefinitionPacket the server send the column definition to the client
func (mp *MysqlProtocolImpl) SendColumnDefinitionPacket(ctx context.Context, column Column, cmd int) error {
	mysqlColumn, ok := column.(*MysqlColumn)
	if !ok {
		return moerr.NewInternalError(ctx, "sendColumn need MysqlColumn")
	}

	var data []byte
	if mp.capability&CLIENT_PROTOCOL_41 != 0 {
		data = mp.makeColumnDefinition41Payload(mysqlColumn, cmd)
	}

	return mp.writePackets(data)
}

// SendColumnCountPacket makes the column count packet
func (mp *MysqlProtocolImpl) SendColumnCountPacket(count uint64) error {
	data := make([]byte, HeaderOffset+20)
	pos := HeaderOffset
	pos = mp.writeIntLenEnc(data, pos, count)

	return mp.writePackets(data[:pos])
}

func (mp *MysqlProtocolImpl) sendColumns(ctx context.Context, mrs *MysqlResultSet, cmd int, warnings, status uint16) error {
	//column_count * Protocol::ColumnDefinition packets
	for i := uint64(0); i < mrs.GetColumnCount(); i++ {
		var col Column
		col, err := mrs.GetColumn(ctx, i)
		if err != nil {
			return err
		}

		err = mp.SendColumnDefinitionPacket(ctx, col, cmd)
		if err != nil {
			return err
		}
	}

	//If the CLIENT_DEPRECATE_EOF client capabilities flag is not set, EOF_Packet
	if mp.capability&CLIENT_DEPRECATE_EOF == 0 {
		err := mp.sendEOFPacket(warnings, status)
		if err != nil {
			return err
		}
	}
	return nil
}

// the server convert every row of the result set into the format that mysql protocol needs
func (mp *MysqlProtocolImpl) makeResultSetBinaryRow(data []byte, mrs *MysqlResultSet, rowIdx uint64) ([]byte, error) {
	data = mp.append(data, defines.OKHeader) // append OkHeader

	ctx := mp.ses.GetRequestContext()

	// get null buffer
	buffer := mp.binaryNullBuffer[:0]
	columnsLength := mrs.GetColumnCount()
	numBytes4Null := (columnsLength + 7 + 2) / 8
	for i := uint64(0); i < numBytes4Null; i++ {
		buffer = append(buffer, 0)
	}
	for i := uint64(0); i < columnsLength; i++ {
		if isNil, err := mrs.ColumnIsNull(ctx, rowIdx, i); err != nil {
			return nil, err
		} else if isNil {
			bytePos := (i + 2) / 8
			bitPos := byte((i + 2) % 8)
			idx := int(bytePos)
			buffer[idx] |= 1 << bitPos
			continue
		}
	}
	data = mp.append(data, buffer...)

	for i := uint64(0); i < columnsLength; i++ {
		if isNil, err := mrs.ColumnIsNull(ctx, rowIdx, i); err != nil {
			return nil, err
		} else if isNil {
			continue
		}

		column, err := mrs.GetColumn(ctx, uint64(i))
		if err != nil {
			return nil, err
		}
		mysqlColumn, ok := column.(*MysqlColumn)
		if !ok {
			return nil, moerr.NewInternalError(mp.ses.requestCtx, "sendColumn need MysqlColumn")
		}

		switch mysqlColumn.ColumnType() {
		case defines.MYSQL_TYPE_TINY:
			if value, err := mrs.GetInt64(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				data = mp.appendUint8(data, uint8(value))
			}
		case defines.MYSQL_TYPE_SHORT, defines.MYSQL_TYPE_YEAR:
			if value, err := mrs.GetInt64(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				data = mp.appendUint16(data, uint16(value))
			}
		case defines.MYSQL_TYPE_INT24, defines.MYSQL_TYPE_LONG:
			if value, err := mrs.GetInt64(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				buffer = mp.appendUint32(buffer, uint32(value))
			}
		case defines.MYSQL_TYPE_LONGLONG:
			if value, err := mrs.GetUint64(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				buffer = mp.appendUint64(buffer, value)
			}
		case defines.MYSQL_TYPE_FLOAT:
			if value, err := mrs.GetFloat64(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				buffer = mp.appendUint32(buffer, math.Float32bits(float32(value)))
			}
		case defines.MYSQL_TYPE_DOUBLE:
			if value, err := mrs.GetFloat64(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				buffer = mp.appendUint64(buffer, math.Float64bits(value))
			}
		case defines.MYSQL_TYPE_VARCHAR, defines.MYSQL_TYPE_VAR_STRING, defines.MYSQL_TYPE_STRING, defines.MYSQL_TYPE_BLOB, defines.MYSQL_TYPE_TEXT, defines.MYSQL_TYPE_JSON:
			if value, err := mrs.GetString(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		// TODO: some type, we use string now. someday need fix it
		case defines.MYSQL_TYPE_DECIMAL:
			if value, err := mrs.GetString(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_UUID:
			if value, err := mrs.GetString(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_DATE:
			if value, err := mrs.GetValue(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				data = mp.appendDate(data, value.(types.Date))
			}
		case defines.MYSQL_TYPE_TIME:
			if value, err := mrs.GetString(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				var t types.Time
				var err error
				idx := strings.Index(value, ".")
				if idx == -1 {
					t, err = types.ParseTime(value, 0)
				} else {
					t, err = types.ParseTime(value, int32(len(value)-idx-1))
				}
				if err != nil {
					data = mp.appendStringLenEnc(data, value)
				} else {
					data = mp.appendTime(data, t)
				}
			}
			if value, err := mrs.GetValue(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				data = mp.appendTime(data, value.(types.Time))
			}
		case defines.MYSQL_TYPE_DATETIME, defines.MYSQL_TYPE_TIMESTAMP:
			if value, err := mrs.GetString(ctx, rowIdx, i); err != nil {
				return nil, err
			} else {
				var dt types.Datetime
				var err error
				idx := strings.Index(value, ".")
				if idx == -1 {
					dt, err = types.ParseDatetime(value, 0)
				} else {
					dt, err = types.ParseDatetime(value, int32(len(value)-idx-1))
				}
				if err != nil {
					data = mp.appendStringLenEnc(data, value)
				} else {
					data = mp.appendDatetime(data, dt)
				}
			}
		// case defines.MYSQL_TYPE_TIMESTAMP:
		// 	if value, err := mrs.GetString(rowIdx, i); err != nil {
		// 		return nil, err
		// 	} else {
		// 		data = mp.appendStringLenEnc(data, value)
		// 	}
		default:
			return nil, moerr.NewInternalError(ctx, "type is not supported in binary text result row")
		}
	}

	return data, nil
}

// the server convert every row of the result set into the format that mysql protocol needs
func (mp *MysqlProtocolImpl) makeResultSetTextRow(data []byte, mrs *MysqlResultSet, r uint64) ([]byte, error) {
	ctx := mp.ses.GetRequestContext()
	for i := uint64(0); i < mrs.GetColumnCount(); i++ {
		column, err := mrs.GetColumn(ctx, i)
		if err != nil {
			return nil, err
		}
		mysqlColumn, ok := column.(*MysqlColumn)
		if !ok {
			return nil, moerr.NewInternalError(mp.ses.requestCtx, "sendColumn need MysqlColumn")
		}

		if isNil, err1 := mrs.ColumnIsNull(ctx, r, i); err1 != nil {
			return nil, err1
		} else if isNil {
			//NULL is sent as 0xfb
			data = mp.appendUint8(data, 0xFB)
			continue
		}

		switch mysqlColumn.ColumnType() {
		case defines.MYSQL_TYPE_JSON:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_BOOL:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_DECIMAL:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_UUID:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_TINY, defines.MYSQL_TYPE_SHORT, defines.MYSQL_TYPE_INT24, defines.MYSQL_TYPE_LONG, defines.MYSQL_TYPE_YEAR:
			if value, err2 := mrs.GetInt64(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				if mysqlColumn.ColumnType() == defines.MYSQL_TYPE_YEAR {
					if value == 0 {
						data = mp.appendStringLenEnc(data, "0000")
					} else {
						data = mp.appendStringLenEncOfInt64(data, value)
					}
				} else {
					data = mp.appendStringLenEncOfInt64(data, value)
				}
			}
		case defines.MYSQL_TYPE_FLOAT:
			if value, err2 := mrs.GetFloat64(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEncOfFloat64(data, value, 32)
			}
		case defines.MYSQL_TYPE_DOUBLE:
			if value, err2 := mrs.GetFloat64(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEncOfFloat64(data, value, 64)
			}
		case defines.MYSQL_TYPE_LONGLONG:
			if uint32(mysqlColumn.Flag())&defines.UNSIGNED_FLAG != 0 {
				if value, err2 := mrs.GetUint64(ctx, r, i); err2 != nil {
					return nil, err2
				} else {
					data = mp.appendStringLenEncOfUint64(data, value)
				}
			} else {
				if value, err2 := mrs.GetInt64(ctx, r, i); err2 != nil {
					return nil, err2
				} else {
					data = mp.appendStringLenEncOfInt64(data, value)
				}
			}
		case defines.MYSQL_TYPE_VARCHAR, defines.MYSQL_TYPE_VAR_STRING, defines.MYSQL_TYPE_STRING, defines.MYSQL_TYPE_BLOB, defines.MYSQL_TYPE_TEXT:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_DATE:
			if value, err2 := mrs.GetValue(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value.(types.Date).String())
			}
		case defines.MYSQL_TYPE_DATETIME:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_TIME:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		case defines.MYSQL_TYPE_TIMESTAMP:
			if value, err2 := mrs.GetString(ctx, r, i); err2 != nil {
				return nil, err2
			} else {
				data = mp.appendStringLenEnc(data, value)
			}
		default:
			return nil, moerr.NewInternalError(mp.ses.requestCtx, "unsupported column type %d ", mysqlColumn.ColumnType())
		}
	}
	return data, nil
}

// the server send group row of the result set as an independent packet
// thread safe
func (mp *MysqlProtocolImpl) SendResultSetTextBatchRow(mrs *MysqlResultSet, cnt uint64) error {
	if cnt == 0 {
		return nil
	}

	mp.GetLock().Lock()
	defer mp.GetLock().Unlock()
	var err error = nil

	for i := uint64(0); i < cnt; i++ {
		if err = mp.sendResultSetTextRow(mrs, i); err != nil {
			return err
		}
	}
	return err
}

func (mp *MysqlProtocolImpl) SendResultSetTextBatchRowSpeedup(mrs *MysqlResultSet, cnt uint64) error {
	if cnt == 0 {
		return nil
	}

	cmd := mp.GetSession().GetCmd()
	mp.GetLock().Lock()
	defer mp.GetLock().Unlock()
	var err error = nil

	binary := false
	// XXX now we known COM_QUERY will use textRow, COM_STMT_EXECUTE use binaryRow
	if CommandType(cmd) == COM_STMT_EXECUTE {
		binary = true
	}

	//make rows into the batch
	for i := uint64(0); i < cnt; i++ {
		err = mp.openRow(nil)
		if err != nil {
			return err
		}
		//begin1 := time.Now()
		if binary {
			_, err = mp.makeResultSetBinaryRow(nil, mrs, i)
		} else {
			_, err = mp.makeResultSetTextRow(nil, mrs, i)
		}
		//mp.makeTime += time.Since(begin1)

		if err != nil {
			//ERR_Packet in case of error
			err1 := mp.sendErrPacket(moerr.ER_UNKNOWN_ERROR, DefaultMySQLState, err.Error())
			if err1 != nil {
				return err1
			}
			return err
		}

		//output into outbuf
		err = mp.closeRow(nil)
		if err != nil {
			return err
		}
	}

	return err
}

// open a new row of the resultset
func (mp *MysqlProtocolImpl) openRow(_ []byte) error {
	if mp.enableLog {
		logutil.Info("openRow")
	}
	return mp.openPacket()
}

// close a finished row of the resultset
func (mp *MysqlProtocolImpl) closeRow(_ []byte) error {
	if mp.enableLog {
		logutil.Info("closeRow")
	}

	err := mp.closePacket(true)
	if err != nil {
		return err
	}

	err = mp.flushOutBuffer()
	if err != nil {
		return err
	}
	return err
}

// flushOutBuffer the data in the outbuf into the network
func (mp *MysqlProtocolImpl) flushOutBuffer() error {
	if mp.enableLog {
		logutil.Info("flush")
	}

	if mp.bytesInOutBuffer >= mp.untilBytesInOutbufToFlush {
		mp.flushCount++
		mp.writeBytes += uint64(mp.bytesInOutBuffer)
		// FIXME: use a suitable timeout value
		err := mp.tcpConn.Flush(0)
		if err != nil {
			return err
		}
		mp.resetFlushOutBuffer()
	}
	return nil
}

// open a new mysql protocol packet
func (mp *MysqlProtocolImpl) openPacket() error {
	if mp.enableLog {
		logutil.Info("openPacket")
	}

	outbuf := mp.tcpConn.OutBuf()
	n := 4
	outbuf.Grow(n)
	writeIdx := outbuf.GetWriteIndex()
	mp.beginWriteIndex = writeIdx
	writeIdx += n
	mp.bytesInOutBuffer += n
	outbuf.SetWriteIndex(writeIdx)
	if mp.enableLog {
		logutil.Infof("openPacket curWriteIdx %d", outbuf.GetWriteIndex())
	}
	return nil
}

// fill the packet with data
func (mp *MysqlProtocolImpl) fillPacket(elems ...byte) error {
	if mp.enableLog {
		logutil.Infof("fillPacket len %d", len(elems))
	}
	outbuf := mp.tcpConn.OutBuf()
	n := len(elems)
	i := 0
	curLen := 0
	hasDataLen := 0
	curDataLen := 0
	var err error
	var buf []byte
	for ; i < n; i += curLen {
		if !mp.isInPacket() {
			err = mp.openPacket()
			if err != nil {
				return err
			}
		}
		//length of data in the packet
		hasDataLen = outbuf.GetWriteIndex() - mp.beginWriteIndex - HeaderLengthOfTheProtocol
		curLen = int(MaxPayloadSize) - hasDataLen
		curLen = Min(curLen, n-i)
		if curLen < 0 {
			return moerr.NewInternalError(mp.ses.requestCtx, "needLen %d < 0. hasDataLen %d n - i %d", curLen, hasDataLen, n-i)
		}
		outbuf.Grow(curLen)
		buf = outbuf.RawBuf()
		writeIdx := outbuf.GetWriteIndex()
		copy(buf[writeIdx:], elems[i:i+curLen])
		writeIdx += curLen
		mp.bytesInOutBuffer += curLen
		outbuf.SetWriteIndex(writeIdx)
		if mp.enableLog {
			logutil.Infof("fillPacket curWriteIdx %d", outbuf.GetWriteIndex())
		}

		//> 16MB, split it
		curDataLen = outbuf.GetWriteIndex() - mp.beginWriteIndex - HeaderLengthOfTheProtocol
		if curDataLen == int(MaxPayloadSize) {
			err = mp.closePacket(i+curLen == n)
			if err != nil {
				return err
			}

			err = mp.flushOutBuffer()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// close a mysql protocol packet
func (mp *MysqlProtocolImpl) closePacket(appendZeroPacket bool) error {
	if mp.enableLog {
		logutil.Info("closePacket")
	}
	if !mp.isInPacket() {
		return nil
	}
	outbuf := mp.tcpConn.OutBuf()
	payLoadLen := outbuf.GetWriteIndex() - mp.beginWriteIndex - 4
	if mp.enableLog {
		logutil.Infof("closePacket curWriteIdx %d", outbuf.GetWriteIndex())
	}
	if payLoadLen < 0 || payLoadLen > int(MaxPayloadSize) {
		return moerr.NewInternalError(mp.ses.requestCtx, "invalid payload len :%d curWriteIdx %d beginWriteIdx %d ",
			payLoadLen, outbuf.GetWriteIndex(), mp.beginWriteIndex)
	}

	buf := outbuf.RawBuf()
	binary.LittleEndian.PutUint32(buf[mp.beginWriteIndex:], uint32(payLoadLen))
	buf[mp.beginWriteIndex+3] = mp.GetSequenceId()

	mp.AddSequenceId(1)

	if appendZeroPacket && payLoadLen == int(MaxPayloadSize) { //last 16MB packet,append a zero packet
		//if the size of the last packet is exactly MaxPayloadSize, a zero-size payload should be sent
		err := mp.openPacket()
		if err != nil {
			return err
		}
		buf = outbuf.RawBuf()
		binary.LittleEndian.PutUint32(buf[mp.beginWriteIndex:], uint32(0))
		buf[mp.beginWriteIndex+3] = mp.GetSequenceId()
		mp.AddSequenceId(1)
	}

	mp.resetPacket()
	return nil
}

/*
*
append the elems into the outbuffer
*/
func (mp *MysqlProtocolImpl) append(_ []byte, elems ...byte) []byte {
	err := mp.fillPacket(elems...)
	if err != nil {
		panic(err)
	}
	return mp.tcpConn.OutBuf().RawBuf()
}

func (mp *MysqlProtocolImpl) appendDatetime(data []byte, dt types.Datetime) []byte {
	if dt.MicroSec() != 0 {
		data = mp.append(data, 11)
		data = mp.appendUint16(data, uint16(dt.Year()))
		data = mp.append(data, dt.Month(), dt.Day(), byte(dt.Hour()), byte(dt.Minute()), byte(dt.Sec()))
		data = mp.appendUint32(data, uint32(dt.MicroSec()))
	} else if dt.Hour() != 0 || dt.Minute() != 0 || dt.Sec() != 0 {
		data = mp.append(data, 7)
		data = mp.appendUint16(data, uint16(dt.Year()))
		data = mp.append(data, dt.Month(), dt.Day(), byte(dt.Hour()), byte(dt.Minute()), byte(dt.Sec()))
	} else {
		data = mp.append(data, 4)
		data = mp.appendUint16(data, uint16(dt.Year()))
		data = mp.append(data, dt.Month(), dt.Day())
	}
	return data
}

func (mp *MysqlProtocolImpl) appendTime(data []byte, t types.Time) []byte {
	if int64(t) == 0 {
		data = mp.append(data, 0)
	} else {
		hour, minute, sec, msec, isNeg := t.ClockFormat()
		day := uint32(hour / 24)
		hour = hour % 24
		if msec != 0 {
			data = mp.append(data, 12)
			if isNeg {
				data = append(data, byte(1))
			} else {
				data = append(data, byte(0))
			}
			data = mp.appendUint32(data, day)
			data = mp.append(data, uint8(hour), minute, sec)
			data = mp.appendUint64(data, msec)
		} else {
			data = mp.append(data, 8)
			if isNeg {
				data = append(data, byte(1))
			} else {
				data = append(data, byte(0))
			}
			data = mp.appendUint32(data, day)
			data = mp.append(data, uint8(hour), minute, sec)
		}
	}
	return data
}

func (mp *MysqlProtocolImpl) appendDate(data []byte, value types.Date) []byte {
	if int32(value) == 0 {
		data = mp.append(data, 0)
	} else {
		data = mp.append(data, 4)
		data = mp.appendUint16(data, value.Year())
		data = mp.append(data, value.Month(), value.Day())
	}
	return data
}

// the server send every row of the result set as an independent packet
// thread safe
func (mp *MysqlProtocolImpl) SendResultSetTextRow(mrs *MysqlResultSet, r uint64) error {
	mp.GetLock().Lock()
	defer mp.GetLock().Unlock()

	return mp.sendResultSetTextRow(mrs, r)
}

// the server send every row of the result set as an independent packet
func (mp *MysqlProtocolImpl) sendResultSetTextRow(mrs *MysqlResultSet, r uint64) error {
	var err error
	err = mp.openRow(nil)
	if err != nil {
		return err
	}
	if _, err = mp.makeResultSetTextRow(nil, mrs, r); err != nil {
		//ERR_Packet in case of error
		err1 := mp.sendErrPacket(moerr.ER_UNKNOWN_ERROR, DefaultMySQLState, err.Error())
		if err1 != nil {
			return err1
		}
		return err
	}

	err = mp.closeRow(nil)
	if err != nil {
		return err
	}

	//begin2 := time.Now()
	//err = mp.writePackets(data)
	//if err != nil {
	//	return moerr.NewInternalError("send result set text row failed. error: %v", err)
	//}
	//mp.sendTime += time.Since(begin2)

	return nil
}

// the server send the result set of execution the client
// the routine follows the article: https://dev.mysql.com/doc/internals/en/com-query-response.html
func (mp *MysqlProtocolImpl) sendResultSet(ctx context.Context, set ResultSet, cmd int, warnings, status uint16) error {
	mysqlRS, ok := set.(*MysqlResultSet)
	if !ok {
		return moerr.NewInternalError(ctx, "sendResultSet need MysqlResultSet")
	}

	//A packet containing a Protocol::LengthEncodedInteger column_count
	err := mp.SendColumnCountPacket(mysqlRS.GetColumnCount())
	if err != nil {
		return err
	}

	if err = mp.sendColumns(ctx, mysqlRS, cmd, warnings, status); err != nil {
		return err
	}

	//One or more ProtocolText::ResultsetRow packets, each containing column_count values
	for i := uint64(0); i < mysqlRS.GetRowCount(); i++ {
		if err = mp.sendResultSetTextRow(mysqlRS, i); err != nil {
			return err
		}
	}

	//If the CLIENT_DEPRECATE_EOF client capabilities flag is set, OK_Packet; else EOF_Packet.
	if mp.capability&CLIENT_DEPRECATE_EOF != 0 {
		err := mp.sendOKPacketWithEof(0, 0, status, 0, "")
		if err != nil {
			return err
		}
	} else {
		err := mp.sendEOFPacket(warnings, status)
		if err != nil {
			return err
		}
	}

	return nil
}

// the server sends the payload to the client
func (mp *MysqlProtocolImpl) writePackets(payload []byte) error {
	//protocol header length
	var headerLen = HeaderOffset
	var header [4]byte

	//position of the first data byte
	var i = headerLen
	var length = len(payload)
	var curLen int
	for ; i < length; i += curLen {
		//var packet []byte = mp.packet[:0]
		curLen = Min(int(MaxPayloadSize), length-i)

		//make mysql client protocol header
		//4 bytes
		//int<3>    the length of payload
		mp.io.WriteUint32(header[:], 0, uint32(curLen))

		//int<1> sequence id
		mp.io.WriteUint8(header[:], 3, mp.GetSequenceId())

		//send packet
		var packet = append(header[:], payload[i:i+curLen]...)

		err := mp.tcpConn.Write(packet, goetty.WriteOptions{Flush: true})
		if err != nil {
			return err
		}
		mp.AddSequenceId(1)

		if i+curLen == length && curLen == int(MaxPayloadSize) {
			//if the size of the last packet is exactly MaxPayloadSize, a zero-size payload should be sent
			header[0] = 0
			header[1] = 0
			header[2] = 0
			header[3] = mp.GetSequenceId()

			//send header / zero-sized packet
			err := mp.tcpConn.Write(header[:], goetty.WriteOptions{Flush: true})
			if err != nil {
				return err
			}

			mp.AddSequenceId(1)
		}
	}
	return nil
}

/*
//ther server reads a part of payload from the connection
//the part may be a whole payload
func (mp *MysqlProtocolImpl) recvPartOfPayload() ([]byte, error) {
	//var length int
	//var header []byte
	//var err error
	//if header, err = mp.io.ReadPacket(4); err != nil {
	//	return nil, moerr.NewInternalError("read header failed.error:%v", err)
	//} else if header[3] != mp.sequenceId {
	//	return nil, moerr.NewInternalError("client sequence id %d != server sequence id %d", header[3], mp.sequenceId)
	//}

	mp.sequenceId++
	//length = int(uint32(header[0]) | uint32(header[1])<<8 | uint32(header[2])<<16)

	var payload []byte
	//if payload, err = mp.io.ReadPacket(length); err != nil {
	//	return nil, moerr.NewInternalError("read payload failed.error:%v", err)
	//}
	return payload, nil
}

//the server read a payload from the connection
func (mp *MysqlProtocolImpl) recvPayload() ([]byte, error) {
	payload, err := mp.recvPartOfPayload()
	if err != nil {
		return nil, err
	}

	//only one part
	if len(payload) < int(MaxPayloadSize) {
		return payload, nil
	}

	//payload has been split into many parts.
	//read them all together
	var part []byte
	for {
		part, err = mp.recvPartOfPayload()
		if err != nil {
			return nil, err
		}

		payload = append(payload, part...)

		//only one part
		if len(part) < int(MaxPayloadSize) {
			break
		}
	}
	return payload, nil
}
*/

/*
generate random ascii string.
Reference to :mysql 8.0.23 mysys/crypt_genhash_impl.cc generate_user_salt(char*,int)
*/
func generate_salt(n int) []byte {
	buf := make([]byte, n)
	rand.Read(buf)
	for i := 0; i < n; i++ {
		buf[i] &= 0x7f
		if buf[i] == 0 || buf[i] == '$' {
			buf[i]++
		}
	}
	return buf
}

func NewMysqlClientProtocol(connectionID uint32, tcp goetty.IOSession, maxBytesToFlush int, SV *config.FrontendParameters) *MysqlProtocolImpl {
	rand.Seed(time.Now().UTC().UnixNano())
	salt := generate_salt(20)

	mysql := &MysqlProtocolImpl{
		ProtocolImpl: ProtocolImpl{
			io:           NewIOPackage(true),
			tcpConn:      tcp,
			salt:         salt,
			connectionID: connectionID,
		},
		charset:          "utf8mb4",
		capability:       DefaultCapability,
		strconvBuffer:    make([]byte, 0, 16*1024),
		lenEncBuffer:     make([]byte, 0, 10),
		binaryNullBuffer: make([]byte, 0, 512),
		rowHandler: rowHandler{
			beginWriteIndex:           0,
			bytesInOutBuffer:          0,
			untilBytesInOutbufToFlush: maxBytesToFlush * 1024,
			enableLog:                 false,
		},
		SV: SV,
	}

	mysql.MakeProfile()

	if SV.EnableTls {
		mysql.capability = mysql.capability | CLIENT_SSL
	}

	mysql.resetPacket()

	return mysql
}
