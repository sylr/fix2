# The benchmark agent of QuickFIX C++: see main.cpp. It links the static
# QuickFIX C++ of the flake, found through QUICKFIX_CPP and
# QUICKFIX_CPP_VERSION, which nix develop sets, or -D of the same names.
cmake_minimum_required(VERSION 3.21)
project(quickfixcpp-agent LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

# The environment wins over the cache: the flake's QuickFIX C++ changes
# path when flake.lock does.
foreach(var QUICKFIX_CPP QUICKFIX_CPP_VERSION)
  if(DEFINED ENV{${var}})
    set(${var} "$ENV{${var}}" CACHE STRING "" FORCE)
  endif()
  if(NOT ${var})
    message(FATAL_ERROR "${var} is not set: build in nix develop, or pass -D${var}=")
  endif()
endforeach()
# QuickFIX C++'s own static library, never one of the system's.
find_path(QUICKFIX_INCLUDE_DIR quickfix/Session.h PATHS "${QUICKFIX_CPP}/include" NO_DEFAULT_PATH NO_CACHE REQUIRED)
find_library(QUICKFIX_LIBRARY NAMES libquickfix.a PATHS "${QUICKFIX_CPP}/lib" NO_DEFAULT_PATH NO_CACHE REQUIRED)
find_package(Threads REQUIRED)

add_executable(quickfixcpp main.cpp)
target_include_directories(quickfixcpp PRIVATE "${QUICKFIX_INCLUDE_DIR}")
target_link_libraries(quickfixcpp PRIVATE "${QUICKFIX_LIBRARY}" Threads::Threads)
target_compile_definitions(quickfixcpp PRIVATE QUICKFIX_CPP_VERSION="${QUICKFIX_CPP_VERSION}")
