package gws

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxzan/gws/internal"
)

// Conn 结构体表示一个 WebSocket 连接
// Conn struct represents a WebSocket connection
type Conn struct {
	// 互斥锁，用于保护共享资源
	// Mutex to protect shared resources
	mu sync.Mutex

	// 会话存储，用于存储会话数据
	// Session storage for storing session data
	ss SessionStorage

	// 用于存储错误的原子值
	// Atomic value for storing errors
	err atomic.Value

	// 标识是否为服务器端
	// Indicates if this is a server-side connection
	isServer bool

	// 子协议
	// Subprotocol
	subprotocol string

	// 底层网络连接
	// Underlying network connection
	conn net.Conn

	// 配置信息
	// Configuration information
	config *Config

	// 缓冲读取器
	// Buffered reader
	br *bufio.Reader

	// 持续帧
	// Continuation frame
	continuationFrame continuationFrame

	// 帧头
	// Frame header
	fh frameHeader

	// 事件处理器
	// Event handler
	handler Event

	// 关闭状态
	// Closed state
	closed uint32

	// 读取队列
	// Read queue
	readQueue channel

	// 写入队列
	// Write queue
	writeQueue workerQueue

	// 压缩器
	// Deflater
	deflater *deflater

	// 数据包发送窗口
	// Data packet send window
	dpsWindow slideWindow

	// 数据包接收窗口
	// Data packet receive window
	cpsWindow slideWindow

	// 每消息压缩
	// Per-message deflate
	pd PermessageDeflate
}

// ReadLoop 循环读取消息. 如果复用了HTTP Server, 建议开启goroutine, 阻塞会导致请求上下文无法被GC.
// Read messages in a loop.
// If HTTP Server is reused, it is recommended to enable goroutine, as blocking will prevent the context from being GC.
func (c *Conn) ReadLoop() {
	// 触发连接打开事件
	// Trigger the connection open event
	c.handler.OnOpen(c)

	// 无限循环读取消息
	// Infinite loop to read messages
	for {
		// 读取消息，如果发生错误则触发错误事件并退出循环
		// Read message, if an error occurs, trigger the error event and exit the loop
		if err := c.readMessage(); err != nil {
			c.emitError(err)
			break
		}
	}

	// 从原子值中加载错误
	// Load error from atomic value
	err, ok := c.err.Load().(error)

	// 触发连接关闭事件
	// Trigger the connection close event
	c.handler.OnClose(c, internal.SelectValue(ok, err, errEmpty))

	// 回收资源
	// Reclaim resources
	if c.isServer {
		// 重置缓冲读取器并放回缓冲池
		// Reset buffered reader and put it back to the buffer pool
		c.br.Reset(nil)
		c.config.brPool.Put(c.br)
		c.br = nil

		// 如果压缩接收窗口启用，放回压缩字典池
		// If compression receive window is enabled, put the compression dictionary back to the pool
		if c.cpsWindow.enabled {
			c.config.cswPool.Put(c.cpsWindow.dict)
			c.cpsWindow.dict = nil
		}

		// 如果压缩发送窗口启用，放回压缩字典池
		// If compression send window is enabled, put the compression dictionary back to the pool
		if c.dpsWindow.enabled {
			c.config.dswPool.Put(c.dpsWindow.dict)
			c.dpsWindow.dict = nil
		}
	}
}

// getCpsDict 返回用于压缩接收窗口的字典
// getCpsDict returns the dictionary for the compression receive window
func (c *Conn) getCpsDict(isBroadcast bool) []byte {
	// 广播模式必须保证每一帧都是相同的内容, 所以不使用上下文接管优化压缩率
	// In broadcast mode, each frame must be the same content, so context takeover is not used to optimize compression ratio
	if isBroadcast {
		return nil
	}

	// 如果是服务器并且服务器上下文接管启用，返回压缩接收窗口的字典
	// If it is a server and server context takeover is enabled, return the dictionary for the compression receive window
	if c.isServer && c.pd.ServerContextTakeover {
		return c.cpsWindow.dict
	}

	// 如果不是服务器并且客户端上下文接管启用，返回压缩接收窗口的字典
	// If it is not a server and client context takeover is enabled, return the dictionary for the compression receive window
	if !c.isServer && c.pd.ClientContextTakeover {
		return c.cpsWindow.dict
	}

	// 否则返回 nil
	// Otherwise, return nil
	return nil
}

// getDpsDict 返回用于压缩发送窗口的字典
// getDpsDict returns the dictionary for the compression send window
func (c *Conn) getDpsDict() []byte {
	// 如果是服务器并且客户端上下文接管启用，返回压缩发送窗口的字典
	// If it is a server and client context takeover is enabled, return the dictionary for the compression send window
	if c.isServer && c.pd.ClientContextTakeover {
		return c.dpsWindow.dict
	}

	// 如果不是服务器并且服务器上下文接管启用，返回压缩发送窗口的字典
	// If it is not a server and server context takeover is enabled, return the dictionary for the compression send window
	if !c.isServer && c.pd.ServerContextTakeover {
		return c.dpsWindow.dict
	}

	// 否则返回 nil
	// Otherwise, return nil
	return nil
}

// isTextValid 检查文本数据的有效性
// isTextValid checks the validity of text data
func (c *Conn) isTextValid(opcode Opcode, payload []byte) bool {
	// 如果配置启用了 UTF-8 检查
	// If the configuration has UTF-8 check enabled
	if c.config.CheckUtf8Enabled {
		// 检查编码是否有效
		// Check if the encoding is valid
		return internal.CheckEncoding(uint8(opcode), payload)
	}

	// 如果未启用 UTF-8 检查，始终返回 true
	// If UTF-8 check is not enabled, always return true
	return true
}

// isClosed 检查连接是否已关闭
// isClosed checks if the connection is closed
func (c *Conn) isClosed() bool {
	return atomic.LoadUint32(&c.closed) == 1
}

// close 关闭连接并存储错误信息
// close closes the connection and stores the error information
func (c *Conn) close(reason []byte, err error) {
	// 存储错误信息
	// Store the error information
	c.err.Store(err)

	// 发送关闭连接的帧
	// Send a frame to close the connection
	_ = c.doWrite(OpcodeCloseConnection, internal.Bytes(reason))

	// 关闭底层网络连接
	// Close the underlying network connection
	_ = c.conn.Close()
}

// emitError 处理并发出错误事件
// emitError handles and emits an error event
func (c *Conn) emitError(err error) {
	// 如果错误为空，直接返回
	// If the error is nil, return immediately
	if err == nil {
		return
	}

	// 使用原子操作检查并设置连接的关闭状态
	// Use atomic operation to check and set the closed state of the connection
	if atomic.CompareAndSwapUint32(&c.closed, 0, 1) {
		// 初始化响应代码和响应错误
		// Initialize response code and response error
		var responseCode = internal.CloseNormalClosure
		var responseErr error = internal.CloseNormalClosure

		// 根据错误类型设置响应代码和响应错误
		// Set response code and response error based on the error type
		switch v := err.(type) {
		case internal.StatusCode:
			// 如果错误类型是 internal.StatusCode，设置响应代码为该状态码
			// If the error type is internal.StatusCode, set the response code to this status code
			responseCode = v
		case *internal.Error:
			// 如果错误类型是 *internal.Error，设置响应代码为该错误的状态码，并设置响应错误为该错误的错误信息
			// If the error type is *internal.Error, set the response code to the status code of this error and set the response error to the error message of this error
			responseCode = v.Code
			responseErr = v.Err
		default:
			// 对于其他类型的错误，直接设置响应错误为该错误
			// For other types of errors, directly set the response error to this error
			responseErr = err
		}

		// 将响应代码转换为字节切片并附加错误信息
		// Convert response code to byte slice and append error message
		var content = responseCode.Bytes()
		content = append(content, err.Error()...)

		// 如果内容长度超过阈值，截断内容
		// If the content length exceeds the threshold, truncate the content
		if len(content) > internal.ThresholdV1 {
			content = content[:internal.ThresholdV1]
		}

		// 关闭连接并传递内容和响应错误
		// Close the connection and pass the content and response error
		c.close(content, responseErr)
	}
}

// emitClose 处理关闭帧并关闭连接
// emitClose handles the close frame and closes the connection
func (c *Conn) emitClose(buf *bytes.Buffer) error {
	// 默认响应代码为正常关闭
	// Default response code is normal closure
	var responseCode = internal.CloseNormalClosure
	// 默认实际代码为正常关闭的 Uint16 值
	// Default real code is the Uint16 value of normal closure
	var realCode = internal.CloseNormalClosure.Uint16()

	// 根据缓冲区长度设置响应代码和实际代码
	// Set response code and real code based on buffer length
	switch buf.Len() {
	case 0:
		// 如果缓冲区长度为 0，设置响应代码和实际代码为 0
		// If buffer length is 0, set response code and real code to 0
		responseCode = 0
		realCode = 0

	case 1:
		// 如果缓冲区长度为 1，设置响应代码为协议错误，并将缓冲区第一个字节作为实际代码
		// If buffer length is 1, set response code to protocol error and use the first byte of the buffer as the real code
		responseCode = internal.CloseProtocolError
		realCode = uint16(buf.Bytes()[0])
		buf.Reset()

	default:
		// 如果缓冲区长度大于 1，读取前两个字节作为实际代码
		// If buffer length is greater than 1, read the first two bytes as the real code
		var b [2]byte
		_, _ = buf.Read(b[0:])
		realCode = binary.BigEndian.Uint16(b[0:])

		// 根据实际代码设置响应代码
		// Set response code based on the real code
		switch realCode {
		case 1004, 1005, 1006, 1014, 1015:
			// 这些代码表示协议错误
			// These codes indicate protocol errors
			responseCode = internal.CloseProtocolError
		default:
			// 检查实际代码是否在有效范围内
			// Check if the real code is within a valid range
			if realCode < 1000 || realCode >= 5000 || (realCode >= 1016 && realCode < 3000) {
				// 如果实际代码小于 1000 或大于等于 5000，或者在 1016 和 3000 之间，设置响应代码为协议错误
				// If the real code is less than 1000 or greater than or equal to 5000, or between 1016 and 3000, set the response code to protocol error
				responseCode = internal.CloseProtocolError
			} else if realCode < 1016 {
				// 如果实际代码小于 1016，设置响应代码为正常关闭
				// If the real code is less than 1016, set the response code to normal closure
				responseCode = internal.CloseNormalClosure
			} else {
				// 否则，将实际代码转换为状态码并设置为响应代码
				// Otherwise, convert the real code to a status code and set it as the response code
				responseCode = internal.StatusCode(realCode)
			}
		}

		// 检查文本数据的有效性
		// Check the validity of text data
		if !c.isTextValid(OpcodeCloseConnection, buf.Bytes()) {
			responseCode = internal.CloseUnsupportedData
		}
	}

	// 如果连接未关闭，关闭连接并存储错误信息
	// If the connection is not closed, close the connection and store the error information
	if atomic.CompareAndSwapUint32(&c.closed, 0, 1) {
		c.close(responseCode.Bytes(), &CloseError{Code: realCode, Reason: buf.Bytes()})
	}

	// 返回正常关闭状态码
	// Return normal closure status code
	return internal.CloseNormalClosure
}

// SetDeadline 设置连接的截止时间
// SetDeadline sets the deadline for the connection
func (c *Conn) SetDeadline(t time.Time) error {
	// 设置底层连接的截止时间
	// Set the deadline for the underlying connection
	err := c.conn.SetDeadline(t)

	// 触发错误处理
	// Emit error handling
	c.emitError(err)

	// 返回错误信息
	// Return the error
	return err
}

// SetReadDeadline 设置读取操作的截止时间
// SetReadDeadline sets the deadline for read operations
func (c *Conn) SetReadDeadline(t time.Time) error {
	// 设置底层连接的读取截止时间
	// Set the read deadline for the underlying connection
	err := c.conn.SetReadDeadline(t)

	// 触发错误处理
	// Emit error handling
	c.emitError(err)

	// 返回错误信息
	// Return the error
	return err
}

// SetWriteDeadline 设置写入操作的截止时间
// SetWriteDeadline sets the deadline for write operations
func (c *Conn) SetWriteDeadline(t time.Time) error {
	// 设置底层连接的写入截止时间
	// Set the write deadline for the underlying connection
	err := c.conn.SetWriteDeadline(t)

	// 触发错误处理
	// Emit error handling
	c.emitError(err)

	// 返回错误信息
	// Return the error
	return err
}

// LocalAddr 返回本地网络地址
// LocalAddr returns the local network address
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr 返回远程网络地址
// RemoteAddr returns the remote network address
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// NetConn 获取底层的 TCP/TLS/KCP 等连接
// NetConn gets the underlying TCP/TLS/KCP... connection
func (c *Conn) NetConn() net.Conn {
	return c.conn
}

// SetNoDelay 控制操作系统是否应该延迟数据包传输以期望发送更少的数据包（Nagle 算法）。
// 默认值是 true（无延迟），这意味着数据在 Write 之后尽快发送。
// SetNoDelay controls whether the operating system should delay packet transmission in hopes of sending fewer packets (Nagle's algorithm).
// The default is true (no delay), meaning that data is sent as soon as possible after a Write.
func (c *Conn) SetNoDelay(noDelay bool) error {
	switch v := c.conn.(type) {
	case *net.TCPConn:
		// 如果底层连接是 TCP 连接，设置无延迟选项
		// If the underlying connection is a TCP connection, set the no delay option
		return v.SetNoDelay(noDelay)

	case *tls.Conn:
		// 如果底层连接是 TLS 连接，获取其底层的 TCP 连接并设置无延迟选项
		// If the underlying connection is a TLS connection, get its underlying TCP connection and set the no delay option
		if netConn, ok := v.NetConn().(*net.TCPConn); ok {
			return netConn.SetNoDelay(noDelay)
		}
	}

	// 如果不是 TCP 或 TLS 连接，返回 nil
	// If it is not a TCP or TLS connection, return nil
	return nil
}

// SubProtocol 获取协商的子协议
// SubProtocol gets the negotiated sub-protocol
func (c *Conn) SubProtocol() string { return c.subprotocol }

// Session 获取会话存储
// Session gets the session storage
func (c *Conn) Session() SessionStorage { return c.ss }
