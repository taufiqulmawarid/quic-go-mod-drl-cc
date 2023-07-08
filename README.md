# Modified quic-go to support congestion control based on deep reinforcement learning

This repository stores the source code of this paper.
> Naqvi, H. A., Hilman, M. H., & Anggorojati, B. (2023). Implementability improvement of deep reinforcement learning based congestion control in cellular network. In Computer Networks (Vol. 233, p. 109874). Elsevier BV. https://doi.org/10.1016/j.comnet.2023.109874

It is the deployment part of congestion control based on deep reinforcement learning within quic as transport protocol. The congestion control basically follows Aurora design with some modification to improve its stability. If you want to train following the paper's procedure, please go to [this repository](https://github.com/haidlir/ns3-drl-cc). However, you can also use the Aurora's trained model in [here](https://github.com/PCCproject/PCC-RL/issues/5#issuecomment-570252217).

> **Note**
> You can find the original quic-go README text in [here](README-original.md).

# How to use
## Dependency
The inference process requires python's tensorflow to decide the next sending rate. You need to setup python 3.7.16 and install the package listed in the [requirements file](example/server_test/py-3.7.16-requirements.txt).
```python
$ pip install -r py-3.7.16-requirements.txt
```

## As a Server
See the [example server](example/server_test) folder. It setups the HTTP 3 listener.
1. Config the [sent_packet_handler](https://github.com/haidlir/quic-go-mod-drl-cc/blob/8dbb38c466ab81253e94243f6b28a87016e2b014/internal/ackhandler/sent_packet_handler.go#L134) to use Aurora as congestion control.

```golang
	// Reproduced PCC Aurora Sender
	congestion := congestion.NewReproducedPccAuroraSender(
		rttStats,
		initialMaxDatagramSize,
		true,
		tracer,
	)
```

2. Extract the trained model into the [saved_models folder](example/server_test/py_aurora/saved_models/).
3. Compile the [main.go file](example/server_test/main.go) to produce a binary file.
4. Run the binary file.
```bash
$ ./<binary-file> -bind 0.0.0.0:6121 -aurora-model ./py_aurora/saved_models/<model-folder> -interval-rtt-n 2.0 -interval-rtt-estimator 1
```

## As a Client
See the [example client](example/client_test) folder.

1. Config the [sent_packet_handler](https://github.com/haidlir/quic-go-mod-drl-cc/blob/8dbb38c466ab81253e94243f6b28a87016e2b014/internal/ackhandler/sent_packet_handler.go#L118) to use the default quic-go's congestion control.
```golang
	// Cubic Sender
	congestion := congestion.NewCubicSender(
		congestion.DefaultClock{},
		rttStats,
		initialMaxDatagramSize,
		true, // use Reno
		tracer,
	)
```
2. Compile the [main.go file](example/client_test/main.go) to produce a binary file.
3. Run the binary file to download dummy data from the server.
```bash
$ ./<binary-file> -insecure https://127.0.0.1:6121/200000000
```

# How to cite
```latex
@article{comnet2023-drlcc-quic,
    title = {Implementability improvement of deep reinforcement learning based congestion control in cellular network},
    journal = {Computer Networks},
    volume = {233},
    pages = {109874},
    year = {2023},
    issn = {1389-1286},
    doi = {https://doi.org/10.1016/j.comnet.2023.109874},
    url = {https://www.sciencedirect.com/science/article/pii/S1389128623003195},
    author = {Haidlir Achmad Naqvi and Muhammad Hafizhuddin Hilman and Bayu Anggorojati}
}
```