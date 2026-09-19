package com.sakurasep.assistant

import android.widget.EditText
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.By
import androidx.test.uiautomator.UiDevice
import androidx.test.uiautomator.Until
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * 真机表单驱动：以无障碍 ACTION_SET_TEXT 直写 Flutter 输入框（绕开中文 IME
 * 对 adb input text 的组词干扰）。凭证经 instrument 参数传入：
 * am instrument -w -e url <hub> -e user <u> -e pass <p> \
 *   -e class com.sakurasep.assistant.DeviceLoginTest \
 *   com.sakurasep.assistant.test/androidx.test.runner.AndroidJUnitRunner
 */
@RunWith(AndroidJUnit4::class)
class DeviceLoginTest {

    @Test
    fun loginViaForm() {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val device = UiDevice.getInstance(instrumentation)
        val args = InstrumentationRegistry.getArguments()
        val url = args.getString("url") ?: error("missing -e url")
        val user = args.getString("user") ?: error("missing -e user")
        val pass = args.getString("pass") ?: error("missing -e pass")

        // instrument 只拉起进程不保证前台 Activity；test 上下文的 startActivity
        // 受后台启动限制，走 shell uid 的 am start 才可靠。
        device.executeShellCommand("am start -n com.sakurasep.assistant/.MainActivity")
        device.waitForIdle(3_000)

        // 登录页三个 EditText 自上而下：中枢地址 / 账号 / 密码。
        val fields = device.wait(
            Until.findObjects(By.clazz(EditText::class.java)), 15_000
        ) ?: error(
            "login fields not found; foreground=${device.currentPackageName} " +
                "nodes=${device.findObjects(By.pkg(device.currentPackageName))?.size ?: -1}"
        )
        assertTrue("expected >=3 EditText, got ${fields.size}", fields.size >= 3)
        // 未聚焦的语义节点 setText 只改语义不改控制器；须先 click 聚焦再对
        // focused 节点写入（经 TextInputConnection 落地到 EditableText）。
        // 键盘弹出会改变布局坐标，后续字段改用 TAB 键遍历焦点而非再点击。
        fields[0].click()
        val values = listOf(url, user, pass)
        for (i in values.indices) {
            val focused = device.wait(
                Until.findObject(By.focused(true)), 5_000
            ) ?: error("field $i did not take focus")
            focused.text = values[i]
            android.util.Log.i("DeviceLoginTest", "field $i set to: ${focused.text}")
            if (i < values.size - 1) {
                device.pressKeyCode(android.view.KeyEvent.KEYCODE_TAB)
                device.waitForIdle(1_000)
            }
        }

        val login = device.wait(
            Until.findObject(By.desc("登录")), 5_000
        ) ?: error("login button not found")
        login.click()

        // 登录成功后跳转主页；等待登录页消失作为通过信号。
        val gone = device.wait(Until.gone(By.desc("登录")), 15_000)
        if (!gone) {
            val visible = device.findObjects(By.pkg("com.sakurasep.assistant"))
                .mapNotNull { it.contentDescription ?: it.text }
                .filter { it.isNotBlank() }
            error("login did not complete; visible texts: $visible")
        }
    }
}
