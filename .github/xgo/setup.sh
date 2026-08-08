#!/usr/bin/env bash

# Go 1.26 export definitions require LLVM MinGW because xgo's GCC linker rejects them.
ln -sf /llvm-mingw/bin/x86_64-w64-mingw32-clang /usr/local/bin/x86_64-w64-mingw32-gcc-posix
ln -sf /llvm-mingw/bin/x86_64-w64-mingw32-clang++ /usr/local/bin/x86_64-w64-mingw32-g++-posix
