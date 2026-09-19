package com.sakurasep.assistant.util

import android.util.Log

/** 原生侧诊断日志开关（setDebugLogging 方法控制），用于通知到达计时验证。 */
object Dbg {
    const val TAG = "AssistantDbg"

    @Volatile
    var enabled: Boolean = false

    /** 常规诊断日志（总是输出，级别 i，供实机取证 grep）。 */
    fun i(msg: String) {
        Log.i(TAG, msg)
    }

    /** 计时日志：仅 enabled 时输出，附带毫秒时间戳便于计算到达时延。 */
    fun timed(msg: String) {
        if (enabled) Log.i(TAG, "[t=${System.currentTimeMillis()}] $msg")
    }

    fun w(msg: String, tr: Throwable? = null) {
        Log.w(TAG, msg, tr)
    }
}
