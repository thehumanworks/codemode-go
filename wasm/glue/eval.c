#include "qjs.h"
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <stdbool.h>

/* Replacements for the two quickjs-libc helpers the glue uses. They drive
 * the job queue only: there are no timers, file descriptors or workers, so a
 * promise nobody can settle is reported instead of spinning forever. */
void js_std_loop(JSContext *ctx)
{
    JSRuntime *rt = JS_GetRuntime(ctx);
    JSContext *ctx1;
    for (;;)
    {
        int err = JS_ExecutePendingJob(rt, &ctx1);
        if (err == 0)
            break; /* queue empty */
        if (err < 0)
            JS_FreeValue(ctx1, JS_GetException(ctx1)); /* a job threw after the result settled: drop it, as quickjs-libc does (it would print to the discarded stderr) */
    }
}

JSValue js_std_await(JSContext *ctx, JSValue obj)
{
    JSRuntime *rt = JS_GetRuntime(ctx);
    for (;;)
    {
        JSPromiseStateEnum state = JS_PromiseState(ctx, obj);
        if (state == JS_PROMISE_FULFILLED)
        {
            JSValue ret = JS_PromiseResult(ctx, obj);
            JS_FreeValue(ctx, obj);
            return ret;
        }
        if (state == JS_PROMISE_REJECTED)
        {
            JSValue ret = JS_Throw(ctx, JS_PromiseResult(ctx, obj));
            JS_FreeValue(ctx, obj);
            return ret;
        }
        if (state != JS_PROMISE_PENDING)
            return obj; /* not a promise */
        JSContext *ctx1;
        int err = JS_ExecutePendingJob(rt, &ctx1);
        if (err < 0)
        {
            JS_FreeValue(ctx, obj);
            return JS_EXCEPTION;
        }
        if (err == 0)
        {
            JS_FreeValue(ctx, obj);
            return JS_ThrowInternalError(ctx, "await: the promise can never settle (nothing is pending and the sandbox has no timers or I/O)");
        }
    }
}

/* import.meta stays empty: there are no files behind modules here. */
static int js_module_set_import_meta(JSContext *ctx, JSValue func_val, bool use_realpath, bool is_main)
{
    return 0;
}

/* Detect and load source code from a file or memory.
 * For file input, returns an allocated buffer (caller must free).
 * For in-memory input, returns the provided buffer.
 */
static uint8_t *detect_buf(JSContext *ctx, QJSEvalOptions *opts, size_t *buf_len, bool is_file)
{
    // If the input is a file, load the file into a buffer.
    // The buffer will be freed by the caller.
    // buf_len will be set to the length of the buffer.
    if (is_file)
    {
        errno = 0;
        return NULL; /* this build has no filesystem */
    }
    // If the input is in-memory, return the provided buffer
    // and set the buffer length
    if (opts->buf)
    {
        *buf_len = strlen(opts->buf);
        if (!opts->filename || opts->filename[0] == '\0')
            opts->filename = "<input>";

        return (uint8_t *)opts->buf;
    }
    // If the input is bytecode, return the provided buffer
    // and set the buffer length
    if (opts->bytecode_buf)
    {
        *buf_len = opts->bytecode_len;
        return (uint8_t *)opts->bytecode_buf;
    }

    return NULL;
}

/* Returns a thrown exception for an empty buffer.
 * If buf is non-NULL, this function returns JS_NULL.
 */
static JSValue buf_empty_error(JSContext *ctx, bool is_file, QJSEvalOptions *opts)
{
    if (JS_HasException(ctx))
        return JS_Throw(ctx, JS_GetException(ctx));

    if (errno != 0)
        return JS_ThrowReferenceError(ctx, "%s: %s", strerror(errno), opts->filename);

    if (is_file)
    {
        if (!opts->filename || opts->filename[0] == '\0')
            return JS_ThrowReferenceError(ctx, "filename is required for file evaluation");
        else
            return JS_ThrowReferenceError(ctx, "could not load file: %s", opts->filename);
    }

    return JS_ThrowReferenceError(ctx, "in-memory buffer/bytecode is required for evaluation");
}

static JSValue load_buf(JSContext *ctx, QJSEvalOptions opts, int flags, bool eval)
{
    bool is_file = input_is_file(opts);
    size_t buf_len = 0;
    uint8_t *buf = detect_buf(ctx, &opts, &buf_len, is_file);
    if (buf == NULL)
        return buf_empty_error(ctx, is_file, &opts);

    JSValue module_val;
    if (opts.bytecode_buf != NULL)
        if (!eval)
        {
            module_val = JS_ReadObject(ctx, opts.bytecode_buf, opts.bytecode_len, JS_READ_OBJ_BYTECODE);
            if (JS_IsException(module_val))
            {
                if (is_file)
                    js_free(ctx, buf);
                return JS_Throw(ctx, JS_GetException(ctx));
            }
        }
        else
        {
            JSValue obj = JS_ReadObject(ctx, opts.bytecode_buf, opts.bytecode_len, JS_READ_OBJ_BYTECODE);
            if (JS_IsException(obj))
            {
                if (is_file)
                    js_free(ctx, buf);
                return JS_Throw(ctx, JS_GetException(ctx));
            }
            module_val = JS_EvalFunction(ctx, obj);
            if (JS_IsException(module_val))
            {
                JS_FreeValue(ctx, obj);
                if (is_file)
                    js_free(ctx, buf);
                return JS_Throw(ctx, JS_GetException(ctx));
            }
        }

    else
    {
        module_val = JS_Eval(ctx, (const char *)buf, buf_len, opts.filename, flags);
        if (JS_IsException(module_val))
        {
            if (is_file)
                js_free(ctx, buf);
            return JS_Throw(ctx, JS_GetException(ctx));
        }
    }

    if (is_file)
        js_free(ctx, buf);

    return module_val;
}

/* Module loader: nothing can be imported in this build. Built-in C modules
 * are resolved before the loader is consulted, and none are registered. */
JSModuleDef *QJS_ModuleLoader(JSContext *ctx, const char *module_name, void *opaque)
{
    JS_ThrowReferenceError(ctx, "could not load module '%s': this sandbox has no module loader", module_name);
    return NULL;
}

/* Loads a module, compiles it (with import.meta support), and resolves it */
JSValue QJS_Load(JSContext *ctx, QJSEvalOptions opts)
{
    if ((opts.eval_flags & JS_EVAL_TYPE_MASK) != JS_EVAL_TYPE_MODULE)
        return JS_ThrowTypeError(ctx, "load only supports module evaluation");

    int eval_flags = (opts.eval_flags ? opts.eval_flags : JS_EVAL_TYPE_MODULE);
    eval_flags |= JS_EVAL_TYPE_MODULE | JS_EVAL_FLAG_COMPILE_ONLY;

    JSValue module_val = load_buf(ctx, opts, eval_flags, false);
    if (JS_IsException(module_val))
        return JS_Throw(ctx, JS_GetException(ctx));

    if (JS_VALUE_GET_TAG(module_val) != JS_TAG_MODULE)
        return JS_ThrowTypeError(ctx, "is not a module");

    if (JS_ResolveModule(ctx, module_val) != 0)
    {
        JSValue ex = JS_HasException(ctx) ? JS_Throw(ctx, JS_GetException(ctx))
                                          : JS_ThrowTypeError(ctx, "resolve module failed");
        JS_FreeValue(ctx, module_val);
        return ex;
    }

    if (js_module_set_import_meta(ctx, module_val, input_is_file(opts), true) < 0)
    {
        JSValue ex = JS_Throw(ctx, JS_GetException(ctx));
        JS_FreeValue(ctx, module_val);
        return ex;
    }

    return module_val;
}

/* Evaluates a module with support for promises and default exports */
static JSValue qjs_eval_module(JSContext *ctx, QJSEvalOptions opts)
{
    JSValue module_val = QJS_Load(ctx, opts);
    if (JS_IsException(module_val))
        return module_val;

    JSValue result = JS_EvalFunction(ctx, module_val);
    if (JS_IsPromise(result))
        result = js_std_await(ctx, result);

    js_std_loop(ctx);
    if (JS_IsException(result))
        return JS_Throw(ctx, JS_GetException(ctx));

    JSModuleDef *module_def = JS_VALUE_GET_PTR(module_val);
    JSValue ns = JS_GetModuleNamespace(ctx, module_def);
    JSAtom default_atom = JS_NewAtom(ctx, "default");
    JSValue default_export = JS_GetProperty(ctx, ns, default_atom);
    JS_FreeAtom(ctx, default_atom);
    JS_FreeValue(ctx, ns);

    if (!JS_IsUndefined(default_export))
    {
        JS_FreeValue(ctx, result);
        return default_export;
    }

    JS_FreeValue(ctx, default_export);
    return result;
}

/* Evaluates code (as a module or global script) with promise handling */
JSValue QJS_Eval(JSContext *ctx, QJSEvalOptions opts)
{
    bool is_module = (opts.eval_flags & JS_EVAL_TYPE_MASK) == JS_EVAL_TYPE_MODULE;
    bool is_compile_only = (opts.eval_flags & JS_EVAL_FLAG_COMPILE_ONLY) > 0;
    JSValue result;

    // compile_only always uses qjs_eval_global
    if (!is_module || is_compile_only)
        result = load_buf(ctx, opts, opts.eval_flags, !is_compile_only);
    else
        result = qjs_eval_module(ctx, opts);

    if (JS_IsException(result))
        return JS_Throw(ctx, JS_GetException(ctx));

    if (JS_IsPromise(result))
        result = js_std_await(ctx, result);

    js_std_loop(ctx);
    if (JS_HasException(ctx))
        return JS_Throw(ctx, JS_GetException(ctx));

    // If the eval is ran in GLOBAL mode with flag JS_EVAL_FLAG_ASYNC
    // The result will be an object of: { value: <result> }
    // We need to extract the value from the object
    bool is_global = (opts.eval_flags & JS_EVAL_TYPE_MASK) == JS_EVAL_TYPE_GLOBAL;
    bool is_async = (opts.eval_flags & JS_EVAL_FLAG_ASYNC) > 0;
    bool is_async_global = is_global && is_async;
    if (is_async_global)
    {
        // JSValue result_json = JS_JSONStringify(ctx, result, JS_NULL, JS_NULL);
        // printf("result is: %s\n", JS_ToCString(ctx, result_json));
        JSAtom value_atom = JS_NewAtom(ctx, "value");
        JSValue value = JS_GetProperty(ctx, result, value_atom);
        JS_FreeAtom(ctx, value_atom);
        JS_FreeValue(ctx, result);
        result = value;
    }

    return result;
}

/*
 * Compiles code (as a module or global script) to bytecode
 * if return value is NULL, an error occurred,
 * use JS_GetException() to retrieve error details.
 */
unsigned char *QJS_Compile(JSContext *c, QJSEvalOptions opts, size_t *outSize)
{
    opts.eval_flags = opts.eval_flags ? opts.eval_flags : JS_EVAL_TYPE_GLOBAL;
    opts.eval_flags |= JS_EVAL_FLAG_COMPILE_ONLY;

    JSValue val = QJS_Eval(c, opts);
    if (JS_IsException(val))
        return NULL;

    size_t psize = 0; // Bytecode size
    unsigned char *tmp_buf = JS_WriteObject(c, &psize, val, JS_WRITE_OBJ_BYTECODE);
    JS_FreeValue(c, val);

    // Validate the output:
    // ensure a positive bytecode size and a valid buffer pointer.
    if (tmp_buf == NULL || (int)psize <= 0)
    {
        if (tmp_buf != NULL)
            js_free(c, tmp_buf);
        return NULL;
    }

    unsigned char *result = (unsigned char *)malloc(psize);
    if (result == NULL)
    {
        js_free(c, tmp_buf);
        return NULL;
    }

    memcpy(result, tmp_buf, psize);
    js_free(c, tmp_buf);
    *outSize = psize;
    return result;
}

/**
 * Compiles code (as a module or global script) to bytecode and returns a packed uint64_t value
 * containing both the bytecode's memory address (high 32 bits) and length (low 32 bits).
 *
 * Returns NULL on error.
 * NOTE: The caller must free both the returned uint64_t pointer and the bytecode memory it points to.
 */
uint64_t *QJS_Compile2(JSContext *ctx, QJSEvalOptions opts)
{
    size_t bytecode_len = 0;
    unsigned char *bytecode = QJS_Compile(ctx, opts, &bytecode_len);
    if (!bytecode)
    {
        return NULL;
    }

    uint64_t *result = malloc(sizeof(uint64_t));
    if (!result)
    {
        free(bytecode);
        return NULL; // Allocation failure
    }

    // Store the address of the bytecode in the high 32 bits and the length in the low 32 bits
    *result = ((uint64_t)(uintptr_t)bytecode << 32) | (uint32_t)bytecode_len;
    return result;
}

QJSEvalOptions *QJS_CreateEvalOption(void *buf, uint8_t *bytecode_buf, size_t bytecode_len, char *filename, int eval_flags)
{
    QJSEvalOptions *opts = (QJSEvalOptions *)malloc(sizeof(QJSEvalOptions));
    if (opts == NULL)
    {
        // Memory allocation failed - return NULL so caller can handle the error
        return NULL;
    }

    opts->buf = buf;
    opts->bytecode_buf = bytecode_buf;
    opts->bytecode_len = bytecode_len;
    opts->filename = filename;
    opts->eval_flags = eval_flags;

    return opts;
}
