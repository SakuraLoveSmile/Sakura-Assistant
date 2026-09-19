package com.sakurasep.assistant

import android.app.Application
import com.sakurasep.assistant.notify.NotificationHelper
import com.sakurasep.assistant.util.Dbg

/** 进程启动即建通知渠道并挂 WorkManager 兜底（worker 内部按 serviceRequested 判定）。 */
class AssistantApp : Application() {
    override fun onCreate() {
        super.onCreate()
        NotificationHelper.ensureChannels(this)
        SyncWatchdogWorker.enqueue(this)
        Dbg.i("AssistantApp onCreate")
    }
}
