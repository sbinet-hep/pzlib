pzlib
=====

Go parallel zlib compression/decompression. This is a fully zlib compatible drop in replacement for "compress/zlib".

The implementation is inspired by [klauspost/pgzip](https://github.com/klauspost/pgzip), and completed as the final project of CMU 15-618: Parallel Computer Architecture and Programming.

According to our experiment, pzlib can achieve a 38× speedup compared to "compress/zlib" on 36-core AWS machines compressing files of 1GB, with moderate file size overhead after compression. Detailed benchmark results can be found in this [report](https://github.com/zianke/15618-final-project/blob/master/final/report.pdf).
