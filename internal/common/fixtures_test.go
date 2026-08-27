package common

// 共享测试固件：各类身份的合法取值，用可辨识的重复数字便于在失败输出里认出来。
// 长度必须与 id.go 声明的一致，否则 Valid() 会拒绝。
const (
	// 设备身份是 UUID 规范形式。A < B 的字典序被 Link 规范化用例依赖。
	testDeviceA DeviceID  = "11111111-1111-4111-8111-111111111111"
	testDeviceB DeviceID  = "22222222-2222-4222-8222-222222222222"
	testNetwork NetworkID = "1234abcd"
	testPeering PeeringID = "33333333333333333333333333333333"
	testToken   LinkToken = "666666666666"
	testSocketA SocketID  = "77777777"
	testSocketB SocketID  = "88888888"
)
