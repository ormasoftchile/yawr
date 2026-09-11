// Package expr contains the runtime expression shim used by production executors.
//
// Phase P8 completed the runtime cutover: boolean evaluation, string
// interpolation, and capture resolution all dispatch to the native GXL/GIS/GCP
// engines. YAWR_EXPR_TRACE=1 still emits JSON selection audit records to stderr.
package expr
