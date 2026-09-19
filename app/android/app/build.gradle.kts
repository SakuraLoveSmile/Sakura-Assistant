import java.io.FileInputStream
import java.util.Properties

plugins {
    id("com.android.application")
    id("kotlin-android")
    id("com.google.devtools.ksp")
    // The Flutter Gradle Plugin must be applied after the Android and Kotlin Gradle plugins.
    id("dev.flutter.flutter-gradle-plugin")
}

// Release 签名材料：android/key.properties（已 gitignore；keystore 在 app/assistant-release.keystore）。
// release 构建缺材料时由 validateReleaseSigning 显式失败，禁止静默回退 debug 签名。
val keystoreProperties = Properties()
val keystorePropertiesFile = rootProject.file("key.properties")
if (keystorePropertiesFile.exists()) {
    FileInputStream(keystorePropertiesFile).use { keystoreProperties.load(it) }
}

// key.properties 完整性检查：缺文件 / 缺键 / storeFile 指向的 keystore 不存在。
fun releaseSigningProblems(): List<String> {
    val problems = mutableListOf<String>()
    if (!keystorePropertiesFile.exists()) {
        problems += "缺少 ${keystorePropertiesFile.path}"
    }
    for (key in listOf("storeFile", "keyAlias", "storePassword", "keyPassword")) {
        if (keystoreProperties.getProperty(key).isNullOrBlank()) {
            problems += "key.properties 缺少键或值为空：$key"
        }
    }
    keystoreProperties.getProperty("storeFile")?.takeIf { it.isNotBlank() }?.let {
        if (!file(it).exists()) problems += "storeFile 指向的 keystore 不存在：${file(it).path}"
    }
    return problems
}

android {
    namespace = "com.sakurasep.assistant"
    // 契约 bridge.md §5：compileSdk/targetSdk 36，minSdk 29（固定值，不随 flutter 默认漂移）
    compileSdk = 36
    ndkVersion = flutter.ndkVersion

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = JavaVersion.VERSION_17.toString()
    }

    defaultConfig {
        applicationId = "com.sakurasep.assistant"
        minSdk = 29
        targetSdk = 36
        versionCode = flutter.versionCode
        versionName = flutter.versionName
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    signingConfigs {
        create("release") {
            keyAlias = keystoreProperties["keyAlias"] as String?
            keyPassword = keystoreProperties["keyPassword"] as String?
            storeFile = (keystoreProperties["storeFile"] as String?)?.let { file(it) }
            storePassword = keystoreProperties["storePassword"] as String?
        }
    }

    buildTypes {
        release {
            // 始终使用正式签名；材料缺失时 validateReleaseSigning 先于打包显式失败。
            signingConfig = signingConfigs.getByName("release")
        }
    }
}

// release 前置校验：签名材料缺失/不完整时明确失败并提示生成方式（仅挂 release 任务图，debug 不受影响）。
val validateReleaseSigning =
    tasks.register("validateReleaseSigning") {
        group = "verification"
        description = "校验 release 签名材料（android/key.properties）是否齐全"
        doLast {
            val problems = releaseSigningProblems()
            if (problems.isNotEmpty()) {
                throw GradleException(
                    buildString {
                        appendLine("release 签名材料不完整，已中止构建：")
                        problems.forEach { appendLine("  - $it") }
                        appendLine()
                        appendLine("生成方式（在 app/android/ 下执行；口令仅写入 key.properties，勿入 Git）：")
                        appendLine("  keytool -genkeypair -v -keystore app/assistant-release.keystore \\")
                        appendLine("    -storetype PKCS12 -alias assistant -keyalg RSA -keysize 4096 -validity 10950")
                        appendLine("  并创建 android/key.properties：")
                        appendLine("    storeFile=assistant-release.keystore")
                        appendLine("    keyAlias=assistant")
                        appendLine("    storePassword=<keystore 口令>")
                        appendLine("    keyPassword=<key 口令>")
                    }
                )
            }
        }
    }

// preReleaseBuild 是 release 变体任务图锚点（assemble/bundle/install 均经过）；packageRelease 兜底。
tasks
    .matching { it.name == "preReleaseBuild" || it.name == "packageRelease" }
    .configureEach { dependsOn(validateReleaseSigning) }

dependencies {
    // SSE 流式读取（bridge.md §3 断线策略、§5 唤醒约束）
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    // 原生持久化 assistant_native.db（bridge.md §4）
    implementation("androidx.room:room-runtime:2.6.1")
    implementation("androidx.room:room-ktx:2.6.1")
    ksp("androidx.room:room-compiler:2.6.1")
    // 15min 断线兜底（bridge.md §3）
    implementation("androidx.work:work-runtime-ktx:2.9.1")
    // ServiceCompat.startForeground / NotificationManagerCompat
    implementation("androidx.core:core-ktx:1.17.0")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")

    testImplementation("junit:junit:4.13.2")

    // UIAutomator 真机表单驱动（绕过 IME 直接 setText，设备验证用）
    androidTestImplementation("androidx.test.ext:junit:1.3.0")
    androidTestImplementation("androidx.test:runner:1.7.0")
    androidTestImplementation("androidx.test.uiautomator:uiautomator:2.3.0")
}

flutter {
    source = "../.."
}
