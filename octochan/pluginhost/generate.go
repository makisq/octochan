package pluginhost

// Перегенерация gRPC-кода из pluginhost/command.proto.
//
// Версии генераторов закреплены намеренно: protoc-gen-go должен совпадать
// с google.golang.org/protobuf из go.mod, иначе сгенерированный файл
// каждый раз меняется целиком (в разных версиях по-разному кодируется
// rawDesc), и коммит превращается в неразличимый шум.
//
// Установка инструментов:
//
//	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.1
//	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
//
// Затем из директории octochan:
//
//	go generate ./pluginhost/...

//go:generate protoc -I.. --go_out=.. --go_opt=paths=source_relative --go-grpc_out=.. --go-grpc_opt=paths=source_relative pluginhost/command.proto
