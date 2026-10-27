这不是给被测代码用的夹具，而是给测试运行器用的。

`internal/web` 的测试通过相对路径 `testdata/marker.txt` 读它。容器里的 runner 必须把
工作目录设回包目录（`/src/internal/web`），这条测试才会通过。

`go test` 原生就是按包目录跑的，所以本机制坏掉时，症状会是「本地/CI 都莫名读不到 testdata」——
把这条测试放在这里，失败点就直指 runner，而不是让人去怀疑被测代码。
