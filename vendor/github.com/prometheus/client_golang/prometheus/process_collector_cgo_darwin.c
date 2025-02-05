// Copyright 2024 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin && cgo

#include <mach/mach_init.h>
#include <mach/task.h>
#include <mach/mach_vm.h>

// The compiler warns that mach/shared_memory_server.h is deprecated, and to use
// mach/shared_region.h instead.  But that doesn't define
// SHARED_DATA_REGION_SIZE or SHARED_TEXT_REGION_SIZE, so redefine them here and
// avoid a warning message when running tests.
#define GLOBAL_SHARED_TEXT_SEGMENT      0x90000000U
#define SHARED_DATA_REGION_SIZE         0x10000000
#define SHARED_TEXT_REGION_SIZE         0x10000000


int get_memory_info(unsigned long long *rss, unsigned long long *vsize)
{
    // This is lightly adapted from how ps(1) obtains its memory info.
    // https://github.com/apple-oss-distributions/adv_cmds/blob/8744084ea0ff41ca4bb96b0f9c22407d0e48e9b7/ps/tasks.c#L109

    kern_return_t               error;
    task_t                      task = MACH_PORT_NULL;
    mach_task_basic_info_data_t info;
    mach_msg_type_number_t      info_count = MACH_TASK_BASIC_INFO_COUNT;

    error = task_info(
                mach_task_self(),
                MACH_TASK_BASIC_INFO,
                (task_info_t) &info,
                &info_count );

    if( error != KERN_SUCCESS )
    {
        return error;
    }

    *rss   = info.resident_size;
    *vsize = info.virtual_size;

    {
        vm_region_basic_info_data_64_t    b_info;
        mach_vm_address_t                 address = GLOBAL_SHARED_TEXT_SEGMENT;
        mach_vm_size_t                    size;
        mach_port_t                       object_name;

        /*
         * try to determine if this task has the split libraries
         * mapped in... if so, adjust its virtual size down by
         * the 2 segments that are used for split libraries
         */
        info_count = VM_REGION_BASIC_INFO_COUNT_64;

        error = mach_vm_region(
                    mach_task_self(),
                    &address,
                    &size,
                    VM_REGION_BASIC_INFO_64,
                    (vm_region_info_t) &b_info,
                    &info_count,
                    &object_name);

        if (error == KERN_SUCCESS) {
            if (b_info.reserved && size == (SHARED_TEXT_REGION_SIZE) &&
                *vsize > (SHARED_TEXT_REGION_SIZE + SHARED_DATA_REGION_SIZE)) {
                    *vsize -= (SHARED_TEXT_REGION_SIZE + SHARED_DATA_REGION_SIZE);
            }
        }
    }

    return 0;
}
