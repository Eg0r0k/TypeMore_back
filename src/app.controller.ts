import { BadRequestException, Body, Controller, Post } from '@nestjs/common';
import { AppService } from './app.service';

@Controller()
export class AppController {
  constructor(private readonly appService: AppService) {}
  @Post('login')
  async login(
    @Body('email') email: string,
    @Body('password_hash') password_hash: string
  ) {
    try {
      const user = await this.appService.findOne(email);

      if (!user) {
        throw new BadRequestException('invalid credentials');
      }

      const isPasswordValid = await this.appService.comparePassword(
        password_hash,
        user.password_hash
      );

      if (!isPasswordValid) {
        throw new BadRequestException('invalid credentials');
      }

      return user;
    } catch (error) {
      throw new BadRequestException('invalid credentials');
    }
  }
}
