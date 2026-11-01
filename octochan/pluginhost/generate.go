package pluginhost

// Перегенерация gRPC-кода из pluginhost/command.proto.
//
// Версии генераторов закреплены намеренно: в разных версиях protoc-gen-go
// по-разному кодируется rawDesc, и перегенерация другой версией меняет файл
// целиком, так что коммит превращается в неразличимый шум. Закреплены те
// версии, которыми сгенерирован текущий код (см. шапки *.pb.go), protoc —
// 30.1. google.golang.org/protobuf в go.mod может быть новее protoc-gen-go:
// новый рантайм принимает код от старого генератора.
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
