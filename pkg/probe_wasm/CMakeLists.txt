cmake_minimum_required(VERSION 3.20)
project(uws_probe_wasm CXX)

enable_testing()

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)  # 探针是性能敏感模块，默认 Release（-O2）
endif()

add_library(uws_probe_core STATIC src/probe.cc src/ast_hash.cc)
target_include_directories(uws_probe_core PUBLIC src)
target_compile_features(uws_probe_core PUBLIC cxx_std_17)
target_compile_options(uws_probe_core PUBLIC -O2)

option(UWS_BUILD_WASM "Build standalone WASM probe target (needs emcmake)" OFF)

# native 逻辑/内存边界用例（宿主 clang++ / ctest）
if(NOT UWS_BUILD_WASM)
add_executable(probe_wasm_test test/probe_wasm_test.cc)
target_link_libraries(probe_wasm_test PRIVATE uws_probe_core)
add_test(NAME probe_wasm_test COMMAND probe_wasm_test)
else()
  # WASM 目标：emcmake cmake -B build/probe_wasm_wasm -DUWS_BUILD_WASM=ON
  # 16MB 线性内存硬边界（-sINITIAL_MEMORY=16777216，禁止增长）；
  # 导出为 standalone wasm（无 JS 胶水，Node/浏览器 WebAssembly.instantiate 直用）。
  add_executable(probe_wasm src/wasm_main.cc)
  target_link_libraries(probe_wasm PRIVATE uws_probe_core)
  target_compile_options(probe_wasm PRIVATE -O2)
  target_link_options(probe_wasm PRIVATE
    --no-entry
    -sSTANDALONE_WASM=1
    -sWASM_BIGINT=1
    -sINITIAL_MEMORY=16777216
    -sALLOW_MEMORY_GROWTH=0
    -sEXPORTED_FUNCTIONS=_uws_probe_reset,_uws_probe_node_begin,_uws_probe_node_field,_uws_probe_node_commit,_uws_probe_nodes_bulk,_uws_probe_finalize,_uws_probe_node_count,_uws_probe_kept_count,_uws_probe_truncated,_uws_probe_needs_fallback,_uws_probe_usage_bp,_uws_probe_last_error,_uws_probe_ast_hash,_uws_probe_scratch,_uws_probe_scratch_size,_uws_probe_arrow_buffer,_uws_probe_copy_node,_uws_probe_overflow_count,_uws_probe_overflow_peek,_uws_probe_export_vlm)
endif()
